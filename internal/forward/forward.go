// Package forward sends Claude Code telemetry on to an OpenTelemetry store
// such as OpenObserve, after Agbredo has checked its token. Agbredo adds the
// run and agent it knows from the token; the agent cannot set those itself.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Resource list key in an OTLP JSON body, per signal.
var resourceKeys = map[string]string{"metrics": "resourceMetrics", "logs": "resourceLogs", "traces": "resourceSpans"}

// Attributes with these prefixes are set only by Agbredo. Any sent by the agent are removed.
var reserved = []string{"agbredo.", "github."}

type item struct {
	signal string
	body   []byte
}

// Forwarder posts telemetry in the background. A nil Forwarder drops everything.
// Forwarding never fails the agent's request: when the store is slow or down,
// telemetry is dropped and the drop is logged.
type Forwarder struct {
	url    string
	auth   string
	client *http.Client
	logger *slog.Logger
	queue  chan item
}

// New returns nil when url is empty. url is the OTLP base, without /v1/...;
// auth is the base64 of user:password for Basic authentication.
func New(url, auth string, logger *slog.Logger) *Forwarder {
	if url == "" {
		return nil
	}
	return &Forwarder{url: strings.TrimRight(url, "/"), auth: auth, client: &http.Client{Timeout: 10 * time.Second}, logger: logger, queue: make(chan item, 100)}
}

// Send labels the body and queues it. signal is metrics, logs or traces.
func (f *Forwarder) Send(signal string, body []byte, labels map[string]string) {
	if f == nil {
		return
	}
	labelled, err := Label(body, resourceKeys[signal], labels)
	if err != nil {
		f.logger.Warn("telemetry not forwarded: cannot label it", "signal", signal)
		return
	}
	select {
	case f.queue <- item{signal, labelled}:
	default:
		f.logger.Warn("telemetry not forwarded: queue full", "signal", signal)
	}
}

// Run posts queued telemetry until ctx ends.
func (f *Forwarder) Run(ctx context.Context) {
	if f == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-f.queue:
			f.post(ctx, it)
		}
	}
}

func (f *Forwarder) post(ctx context.Context, it item) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url+"/v1/"+it.signal, bytes.NewReader(it.body))
	if err != nil {
		f.logger.Warn("telemetry not forwarded", "signal", it.signal, "error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+f.auth)
	resp, err := f.client.Do(req)
	if err != nil {
		f.logger.Warn("telemetry not forwarded", "signal", it.signal, "error", err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		f.logger.Warn("telemetry not forwarded", "signal", it.signal, "status", resp.StatusCode)
	}
}

// Label adds labels to the resource of each entry under key, replacing any
// reserved attributes the sender put there.
func Label(body []byte, key string, labels map[string]string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber() // keeps large integers exact
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	entries, _ := doc[key].([]any)
	for _, entry := range entries {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		resource, _ := e["resource"].(map[string]any)
		if resource == nil {
			resource = map[string]any{}
			e["resource"] = resource
		}
		old, _ := resource["attributes"].([]any)
		attributes := []any{}
		for _, a := range old {
			if m, ok := a.(map[string]any); ok && isReserved(m["key"]) {
				continue
			}
			attributes = append(attributes, a)
		}
		for _, name := range names {
			attributes = append(attributes, map[string]any{"key": name, "value": map[string]any{"stringValue": labels[name]}})
		}
		resource["attributes"] = attributes
	}
	return json.Marshal(doc)
}

func isReserved(key any) bool {
	k, _ := key.(string)
	for _, prefix := range reserved {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}
