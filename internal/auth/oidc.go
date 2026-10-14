package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agbredo/internal/core"
	"github.com/golang-jwt/jwt/v4"
)

const issuer = "https://token.actions.githubusercontent.com"

type OIDC struct {
	audience string
	client   *http.Client
	jwksURL  string
	mu       sync.Mutex
	keys     map[string]*rsa.PublicKey
	fetched  time.Time
}
type runnerClaims struct {
	jwt.RegisteredClaims
	Repository  string `json:"repository"`
	WorkflowRef string `json:"workflow_ref"`
	// JobWorkflowRef names the pipeline (reusable workflow) that ran. It is parsed
	// so the pipeline can be recorded later (ag-s99.2). Never use it for identity.
	JobWorkflowRef string `json:"job_workflow_ref"`
	Ref            string `json:"ref"`
	RunID          string `json:"run_id"`
	RunAttempt     string `json:"run_attempt"`
	EventName      string `json:"event_name"`
}

func NewOIDC(audience string) *OIDC {
	return &OIDC{audience: audience, jwksURL: issuer + "/.well-known/jwks", client: &http.Client{Timeout: 10 * time.Second}, keys: map[string]*rsa.PublicKey{}}
}

func (o *OIDC) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if key := o.keys[kid]; key != nil && time.Since(o.fetched) < 10*time.Minute {
		return key, nil
	}
	// Unknown key IDs cannot force an outbound fetch on every unauthenticated request.
	if !o.fetched.IsZero() && time.Since(o.fetched) < time.Minute {
		return nil, errors.New("unknown GitHub OIDC signing key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch GitHub OIDC signing keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("GitHub OIDC signing keys unavailable")
	}
	var doc struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, errors.New("invalid GitHub OIDC signing key response")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, j := range doc.Keys {
		if j.KTY != "RSA" || j.Alg != "RS256" || (j.Use != "" && j.Use != "sig") || j.KID == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(j.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(j.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 || exponent < 3 || exponent%2 == 0 {
			continue
		}
		keys[j.KID] = &rsa.PublicKey{N: modulus, E: exponent}
	}
	if len(keys) == 0 {
		return nil, errors.New("GitHub OIDC returned no supported signing keys")
	}
	o.keys = keys
	o.fetched = time.Now()
	if keys[kid] == nil {
		return nil, errors.New("unknown GitHub OIDC signing key")
	}
	return keys[kid], nil
}

// Verify returns signed runner identity only. The caller must bind this identity
// to the registered repository/workflow/ref, correlation and actual GitHub run.
// A runner can never use its OIDC identity as an administrator session.
func (o *OIDC) Verify(ctx context.Context, raw string) (core.Identity, error) {
	fail := func() (core.Identity, error) { return core.Identity{}, core.ErrUnauthorized }
	if len(raw) == 0 || len(raw) > 16384 || o.audience == "" {
		return fail()
	}
	claims := &runnerClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}))
	token, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || len(kid) == 0 || len(kid) > 256 {
			return nil, core.ErrUnauthorized
		}
		return o.key(ctx, kid)
	})
	if err != nil || !token.Valid {
		return fail()
	}
	now := time.Now()
	if claims.Issuer != issuer || len(claims.Audience) != 1 || claims.Audience[0] != o.audience || claims.Subject == "" || claims.ID == "" || claims.ExpiresAt == nil || claims.IssuedAt == nil {
		return fail()
	}
	if !claims.ExpiresAt.After(now) || claims.IssuedAt.After(now.Add(30*time.Second)) || claims.IssuedAt.Before(now.Add(-10*time.Minute)) || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > 10*time.Minute || !claims.ExpiresAt.After(claims.IssuedAt.Time) {
		return fail()
	}
	if claims.EventName == "" || claims.EventName == "pull_request" || claims.EventName == "pull_request_target" || claims.Repository == "" || len(strings.Split(claims.Repository, "/")) != 2 || !strings.HasPrefix(claims.Ref, "refs/") {
		return fail()
	}
	prefix := claims.Repository + "/.github/workflows/"
	suffix := "@" + claims.Ref
	if !strings.HasPrefix(claims.WorkflowRef, prefix) || !strings.HasSuffix(claims.WorkflowRef, suffix) {
		return fail()
	}
	workflow := strings.TrimSuffix(strings.TrimPrefix(claims.WorkflowRef, prefix), suffix)
	if workflow == "" || strings.ContainsAny(workflow, "/\\") || (!strings.HasSuffix(workflow, ".yml") && !strings.HasSuffix(workflow, ".yaml")) {
		return fail()
	}
	// Reusable workflows are accepted: job_workflow_ref may name a pipeline in
	// any repository. The identity always comes from workflow_ref, the caller
	// workflow in the target repository, never from job_workflow_ref.
	runID, err := strconv.ParseInt(claims.RunID, 10, 64)
	if err != nil || runID <= 0 {
		return fail()
	}
	attempt, err := strconv.ParseInt(claims.RunAttempt, 10, 64)
	if err != nil || attempt <= 0 {
		return fail()
	}
	return core.Identity{EventName: claims.EventName, Repository: claims.Repository, Workflow: workflow, Ref: claims.Ref, RunID: runID, RunAttempt: attempt}, nil
}
