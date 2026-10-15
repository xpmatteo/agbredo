// Package forward sends Claude Code telemetry on to an OpenTelemetry store
// such as OpenObserve, after Agbredo has checked its token. Agbredo adds the
// run and agent it knows from the token; the agent cannot set those itself.
// It also archives each verified GitHub webhook delivery there, as a log record.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Resource list key in an OTLP JSON body, per signal.
var resourceKeys = map[string]string{"metrics": "resourceMetrics", "logs": "resourceLogs", "traces": "resourceSpans"}

// Attributes with these prefixes are set only by Agbredo. Any sent by the agent are removed.
var reserved = []string{"agbredo.", "github."}

// WebhookStream is the OpenObserve stream that keeps webhook deliveries,
// apart from Claude Code's logs. Other stores ignore the header.
const WebhookStream = "github_webhooks"

type item struct {
	signal string
	body   []byte
	stream string // OpenObserve stream; empty means the store's default
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
	// A pasted secret often ends in a newline, which is not allowed in a header.
	return &Forwarder{url: strings.TrimRight(url, "/"), auth: strings.TrimSpace(auth), client: &http.Client{Timeout: 10 * time.Second}, logger: logger, queue: make(chan item, 100)}
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
	f.enqueue(item{signal: signal, body: labelled})
}

// SendWebhook archives one verified webhook delivery as one OTLP log record.
// The body is kept unchanged as the record's body. attrs are added to the
// record, next to github.delivery and github.event; empty ones are left out.
func (f *Forwarder) SendWebhook(delivery, event string, body []byte, attrs map[string]string) {
	if f == nil {
		return
	}
	record, err := WebhookRecord(delivery, event, time.Now(), body, attrs)
	if err != nil {
		f.logger.Warn("webhook not archived", "delivery", delivery, "error", err.Error())
		return
	}
	f.enqueue(item{signal: "logs", body: record, stream: WebhookStream})
}

func (f *Forwarder) enqueue(it item) {
	select {
	case f.queue <- it:
	default:
		f.logger.Warn("telemetry not forwarded: queue full", "signal", it.signal)
	}
}

// WebhookRecord builds the OTLP JSON logs body for one webhook delivery.
func WebhookRecord(delivery, event string, received time.Time, body []byte, attrs map[string]string) ([]byte, error) {
	all := map[string]string{"github.delivery": delivery, "github.event": event}
	for k, v := range attrs {
		all[k] = v
	}
	names := make([]string, 0, len(all))
	for name, value := range all {
		if value != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	attributes := []any{}
	for _, name := range names {
		attributes = append(attributes, attribute(name, all[name]))
	}
	at := strconv.FormatInt(received.UnixNano(), 10)
	doc := map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{attribute("service.name", "agbredo-github-webhooks")}},
		"scopeLogs": []any{map[string]any{
			"scope": map[string]any{"name": "agbredo"},
			"logRecords": []any{map[string]any{
				"timeUnixNano": at, "observedTimeUnixNano": at,
				"severityNumber": 9, "severityText": "INFO",
				"body":       map[string]any{"stringValue": string(body)},
				"attributes": attributes,
			}},
		}},
	}}}
	return json.Marshal(doc)
}

func attribute(key, value string) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"stringValue": value}}
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
	if it.stream != "" {
		req.Header.Set("stream-name", it.stream)
	}
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
	if key == "resourceLogs" {
		doublesToStrings(doc)
	}
	return json.Marshal(doc)
}

// doublesToStrings turns every {"doubleValue": n} into {"stringValue": "n"}.
// OpenObserve (v1.0.4) refuses OTLP JSON logs with a decimal doubleValue,
// such as Claude Code's cost_usd. All are turned, not only decimals, so a
// field always has the same type. Integer fields like cost_usd_micros stay.
func doublesToStrings(x any) {
	switch v := x.(type) {
	case map[string]any:
		if n, ok := v["doubleValue"].(json.Number); ok && len(v) == 1 {
			delete(v, "doubleValue")
			v["stringValue"] = n.String()
			return
		}
		for _, child := range v {
			doublesToStrings(child)
		}
	case []any:
		for _, child := range v {
			doublesToStrings(child)
		}
	}
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
