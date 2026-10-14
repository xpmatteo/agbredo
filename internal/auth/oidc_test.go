package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

func TestOIDCAuthenticatesOnlyBoundShortLivedWorkflowTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fetches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kid": "key1", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	defer server.Close()
	o := NewOIDC("https://agbredo.example")
	o.jwksURL = server.URL
	valid := func() jwt.MapClaims {
		now := time.Now()
		return jwt.MapClaims{"iss": issuer, "aud": "https://agbredo.example", "sub": "repo:o/r:ref:refs/heads/main", "jti": "token-id", "iat": now.Add(-time.Second).Unix(), "nbf": now.Add(-time.Second).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "repository": "o/r", "workflow_ref": "o/r/.github/workflows/agent.yml@refs/heads/main", "ref": "refs/heads/main", "run_id": "123", "run_attempt": "1", "event_name": "workflow_dispatch"}
	}
	sign := func(claims jwt.MapClaims, kid string, method jwt.SigningMethod) string {
		token := jwt.NewWithClaims(method, claims)
		token.Header["kid"] = kid
		var signingKey any = key
		if method != jwt.SigningMethodRS256 {
			signingKey = []byte("wrong")
		}
		raw, err := token.SignedString(signingKey)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	identity, err := o.Verify(context.Background(), sign(valid(), "key1", jwt.SigningMethodRS256))
	if err != nil {
		t.Fatal(err)
	}
	if identity.Repository != "o/r" || identity.Workflow != "agent.yml" || identity.Ref != "refs/heads/main" || identity.RunID != 123 || identity.RunAttempt != 1 {
		t.Fatalf("identity mismatch %#v", identity)
	}
	for _, event := range []string{"schedule", "push", "repository_dispatch", "issues", "issue_comment"} {
		claims := valid()
		claims["event_name"] = event
		identity, err := o.Verify(context.Background(), sign(claims, "key1", jwt.SigningMethodRS256))
		if err != nil || identity.EventName != event {
			t.Fatalf("existing trigger rejected: %s %v", event, err)
		}
	}
	cases := []struct {
		name   string
		change func(jwt.MapClaims)
	}{
		{"issuer", func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }},
		{"audience", func(c jwt.MapClaims) { c["aud"] = "other" }},
		{"multiple audiences", func(c jwt.MapClaims) { c["aud"] = []string{"https://agbredo.example", "other"} }},
		{"expired", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{"missing expiry", func(c jwt.MapClaims) { delete(c, "exp") }},
		{"old issuance", func(c jwt.MapClaims) { c["iat"] = time.Now().Add(-time.Hour).Unix() }},
		{"long TTL", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(time.Hour).Unix() }},
		{"workflow repository mismatch", func(c jwt.MapClaims) { c["repository"] = "evil/r" }},
		{"workflow ref mismatch", func(c jwt.MapClaims) { c["ref"] = "refs/heads/other" }},
		{"workflow traversal", func(c jwt.MapClaims) { c["workflow_ref"] = "o/r/.github/workflows/../agent.yml@refs/heads/main" }},
		{"pull request", func(c jwt.MapClaims) { c["event_name"] = "pull_request" }},
		{"invalid run", func(c jwt.MapClaims) { c["run_id"] = "0" }},
		{"invalid attempt", func(c jwt.MapClaims) { c["run_attempt"] = "0" }},
		{"missing replay identity", func(c jwt.MapClaims) { delete(c, "jti") }},
		{"caller workflow outside repository", func(c jwt.MapClaims) {
			c["workflow_ref"] = "org/agbredo-agents/.github/workflows/triage.yml@refs/heads/main"
			c["job_workflow_ref"] = "o/r/.github/workflows/agent.yml@refs/heads/main"
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			c := valid()
			test.change(c)
			if _, err := o.Verify(context.Background(), sign(c, "key1", jwt.SigningMethodRS256)); err == nil {
				t.Fatal("untrusted token accepted")
			}
		})
	}
	// A reusable workflow: the caller workflow in o/r calls a pipeline in a
	// central repository. The token is accepted, and the identity comes from
	// workflow_ref (the caller), never from job_workflow_ref (the pipeline).
	reusable := valid()
	reusable["job_workflow_ref"] = "org/agbredo-agents/.github/workflows/triage.yml@refs/tags/v1"
	identity, err = o.Verify(context.Background(), sign(reusable, "key1", jwt.SigningMethodRS256))
	if err != nil {
		t.Fatalf("reusable workflow rejected: %v", err)
	}
	if identity.Repository != "o/r" || identity.Workflow != "agent.yml" || identity.Ref != "refs/heads/main" {
		t.Fatalf("identity must come from workflow_ref, got %#v", identity)
	}
	// A direct job: job_workflow_ref equals workflow_ref, as in real tokens.
	direct := valid()
	direct["job_workflow_ref"] = direct["workflow_ref"]
	identity, err = o.Verify(context.Background(), sign(direct, "key1", jwt.SigningMethodRS256))
	if err != nil {
		t.Fatalf("direct job rejected: %v", err)
	}
	if identity.Repository != "o/r" || identity.Workflow != "agent.yml" || identity.Ref != "refs/heads/main" {
		t.Fatalf("direct job identity mismatch %#v", identity)
	}
	// A reusable workflow in the same repository: the identity is still the caller.
	sameRepo := valid()
	sameRepo["workflow_ref"] = "o/r/.github/workflows/other.yml@refs/heads/main"
	sameRepo["job_workflow_ref"] = "o/r/.github/workflows/agent.yml@refs/heads/main"
	identity, err = o.Verify(context.Background(), sign(sameRepo, "key1", jwt.SigningMethodRS256))
	if err != nil {
		t.Fatalf("same-repository reusable workflow rejected: %v", err)
	}
	if identity.Repository != "o/r" || identity.Workflow != "other.yml" || identity.Ref != "refs/heads/main" {
		t.Fatalf("identity must come from workflow_ref, got %#v", identity)
	}
	for _, raw := range []string{sign(valid(), "unknown", jwt.SigningMethodRS256), sign(valid(), "key1", jwt.SigningMethodHS256), strings.Repeat("x", 20000), "invalid"} {
		if _, err := o.Verify(context.Background(), raw); err == nil {
			t.Fatal("invalid signature accepted")
		}
	}
	if fetches != 1 {
		t.Fatalf("JWKS cache fetched %d times", fetches)
	}
}
