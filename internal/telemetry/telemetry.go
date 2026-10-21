// Package telemetry receives Claude Code OTLP telemetry from Pipeline runs
// and forwards it, unchanged, to OpenObserve.
//
// A run proves who it is with a GitHub OIDC token. We accept it when the job
// runs a Pipeline from one of the trusted repositories. We do not read the
// OTLP body, so the run attributes in it are what the workflow says.
package telemetry

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

const (
	Issuer   = "https://token.actions.githubusercontent.com"
	JWKSURL  = Issuer + "/.well-known/jwks"
	Audience = "agbredo"
)

type claims struct {
	jwt.RegisteredClaims
	// JobWorkflowRef names the reusable workflow (the Pipeline) the job runs,
	// e.g. xpmatteo/agbredo-pipelines/.github/workflows/audit.yml@refs/tags/v1
	JobWorkflowRef string `json:"job_workflow_ref"`
}

// Handler serves POST /api/otlp/v1/{signal}, e.g. /api/otlp/v1/metrics.
type Handler struct {
	Keyfunc jwt.Keyfunc
	// PipelineRepos are the repositories, as owner/name, whose Pipelines may send telemetry.
	PipelineRepos []string
	proxy         *httputil.ReverseProxy
}

// New forwards to target, e.g. https://api.openobserve.ai/api/my-org, with
// authorization as the Authorization header, e.g. "Basic ...".
// pipelineRepos is a comma-separated list of owner/name.
func New(keyfunc jwt.Keyfunc, pipelineRepos string, target *url.URL, authorization string) *Handler {
	var repos []string
	for _, repo := range strings.Split(pipelineRepos, ",") {
		if repo = strings.TrimSpace(repo); repo != "" {
			repos = append(repos, repo)
		}
	}
	return &Handler{Keyfunc: keyfunc, PipelineRepos: repos, proxy: &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + strings.TrimPrefix(r.In.URL.Path, "/api/otlp")
			r.Out.URL.RawPath = ""
			r.Out.Header.Set("Authorization", authorization)
		},
	}}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pipeline, err := h.verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		log.Printf("reject telemetry: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !h.trusted(pipeline) {
		log.Printf("reject telemetry: untrusted pipeline %q", pipeline)
		http.Error(w, "untrusted pipeline", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	h.proxy.ServeHTTP(w, r)
}

// verify returns the job_workflow_ref of a valid token.
func (h *Handler) verify(raw string) (string, error) {
	c := &claims{}
	_, err := jwt.ParseWithClaims(raw, c, h.Keyfunc, jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(Issuer), jwt.WithAudience(Audience), jwt.WithExpirationRequired())
	return c.JobWorkflowRef, err
}

func (h *Handler) trusted(pipeline string) bool {
	for _, repo := range h.PipelineRepos {
		if strings.HasPrefix(pipeline, repo+"/.github/workflows/") {
			return true
		}
	}
	return false
}
