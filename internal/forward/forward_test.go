package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func resourceAttributes(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []struct {
					Key   string         `json:"key"`
					Value map[string]any `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, a := range doc.ResourceLogs[0].Resource.Attributes {
		got[a.Key] = a.Value["stringValue"]
	}
	return got
}

func TestLabelReplacesReservedAttributesAndKeepsTheRest(t *testing.T) {
	body := []byte(`{"resourceLogs":[{"resource":{"attributes":[
		{"key":"service.name","value":{"stringValue":"claude-code"}},
		{"key":"agbredo.run.id","value":{"stringValue":"forged"}},
		{"key":"github.repository","value":{"stringValue":"forged/repo"}}]},
		"scopeLogs":[{"logRecords":[{"timeUnixNano":"1760000000000000000","attributes":[{"key":"n","value":{"intValue":9007199254740993}}]}]}]}]}`)
	out, err := Label(body, "resourceLogs", map[string]string{"agbredo.run.id": "run1", "github.repository": "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	got := resourceAttributes(t, out)
	want := map[string]any{"service.name": "claude-code", "agbredo.run.id": "run1", "github.repository": "o/r"}
	if len(got) != len(want) {
		t.Fatalf("attributes %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v", k, got[k], v)
		}
	}
	// Large integers must not be rounded through float64.
	if !json.Valid(out) || !bytes.Contains(out, []byte("9007199254740993")) {
		t.Fatalf("number changed: %s", out)
	}
}

func TestLabelAddsAResourceWhenMissing(t *testing.T) {
	out, err := Label([]byte(`{"resourceLogs":[{"scopeLogs":[]}]}`), "resourceLogs", map[string]string{"agbredo.source": "review"})
	if err != nil {
		t.Fatal(err)
	}
	if resourceAttributes(t, out)["agbredo.source"] != "review" {
		t.Fatalf("label missing: %s", out)
	}
}

func TestForwarderPostsWithBasicAuth(t *testing.T) {
	type request struct{ path, auth, contentType string }
	got := make(chan request, 1)
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- request{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")}
	}))
	defer store.Close()
	f := New(store.URL+"/api/default/", "c2VjcmV0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx)
	f.Send("logs", []byte(`{"resourceLogs":[]}`), nil)
	select {
	case r := <-got:
		if r.path != "/api/default/v1/logs" || r.auth != "Basic c2VjcmV0" || r.contentType != "application/json" {
			t.Fatalf("unexpected request %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing forwarded")
	}
}

func TestNilForwarderDropsEverything(t *testing.T) {
	var f *Forwarder = New("", "", nil)
	f.Send("logs", []byte(`{}`), nil)
	f.Run(context.Background())
}
