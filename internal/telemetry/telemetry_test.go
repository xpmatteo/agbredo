package telemetry

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var key, _ = rsa.GenerateKey(rand.Reader, 2048)

func keyfunc(*jwt.Token) (any, error) { return &key.PublicKey, nil }

func token(t *testing.T, change func(jwt.MapClaims)) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": Issuer, "aud": Audience, "exp": time.Now().Add(5 * time.Minute).Unix(),
		"job_workflow_ref": "xpmatteo/agbredo-pipelines/.github/workflows/audit.yml@refs/tags/v1",
	}
	if change != nil {
		change(c)
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, c).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type received struct {
	path, authorization, contentType, body string
}

func setup(t *testing.T) (*Handler, *[]received) {
	t.Helper()
	var got []received
	openObserve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, received{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(openObserve.Close)
	target, _ := url.Parse(openObserve.URL + "/api/my-org")
	return New(keyfunc, "xpmatteo/agbredo, xpmatteo/agbredo-pipelines", target, "Basic b3BlbjpvYnNlcnZl"), &got
}

func post(h http.Handler, authorization string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/otlp/v1/metrics", strings.NewReader("otlp bytes"))
	r.Header.Set("Content-Type", "application/x-protobuf")
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestForwardsTelemetryOfATrustedPipeline(t *testing.T) {
	h, got := setup(t)
	if w := post(h, "Bearer "+token(t, nil)); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	want := received{"/api/my-org/v1/metrics", "Basic b3BlbjpvYnNlcnZl", "application/x-protobuf", "otlp bytes"}
	if len(*got) != 1 || (*got)[0] != want {
		t.Fatalf("got  %+v\nwant %+v", *got, want)
	}
}

func TestAcceptsTokensThatExpiredDuringTheJob(t *testing.T) {
	h, got := setup(t)
	expired := token(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-MaxJob + time.Minute).Unix() })
	if w := post(h, "Bearer "+expired); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(*got) != 1 {
		t.Fatalf("forwarded %+v", *got)
	}
}

func TestRejectsBadTokens(t *testing.T) {
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	signedByOther, _ := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": Issuer, "aud": Audience, "exp": time.Now().Add(time.Minute).Unix(),
		"job_workflow_ref": "xpmatteo/agbredo/.github/workflows/sample-pipeline.yml@refs/heads/main",
	}).SignedString(other)
	h, got := setup(t)
	for name, authorization := range map[string]string{
		"missing":          "",
		"not a token":      "Bearer nonsense",
		"other key":        "Bearer " + signedByOther,
		"expired long ago": "Bearer " + token(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-MaxJob - time.Minute).Unix() }),
		"no expiry":        "Bearer " + token(t, func(c jwt.MapClaims) { delete(c, "exp") }),
		"other issuer":     "Bearer " + token(t, func(c jwt.MapClaims) { c["iss"] = "https://example.com" }),
		"other audience":   "Bearer " + token(t, func(c jwt.MapClaims) { c["aud"] = "sts.amazonaws.com" }),
		"without Bearer ":  "Basic " + token(t, nil),
	} {
		if w := post(h, authorization); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, w.Code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("forwarded %+v", *got)
	}
}

func TestRejectsUntrustedPipelines(t *testing.T) {
	h, got := setup(t)
	for _, ref := range []string{
		"",
		"someone/else/.github/workflows/audit.yml@refs/heads/main",
		"xpmatteo/agbredo-pipelines-fork/.github/workflows/audit.yml@refs/heads/main",
	} {
		if w := post(h, "Bearer "+token(t, func(c jwt.MapClaims) { c["job_workflow_ref"] = ref })); w.Code != http.StatusForbidden {
			t.Errorf("%q: status %d", ref, w.Code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("forwarded %+v", *got)
	}
}
