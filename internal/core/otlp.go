package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// The OTLP parsers keep only numbers and counts, and three short labels: the
// Claude Code version, the model and the effort level, each kept only if it
// has a strict form. Other attribute values, error text, account identities
// and everything else in the payload are hashed for deduplication or dropped.

type metricSample struct {
	Series     string // identifies one counter: resource, scope, metric, attributes, start time
	Key        string // identifies one exported value of that counter
	Field      string // input, output, cacheRead, cacheCreation or cost
	Cumulative bool
	End        int64
	Amount     float64
	Model      string // the model attribute, if it has the form of one
	Effort     string // the effort attribute, if it has the form of one
}

type logEvent struct {
	Key       string
	RateLimit bool
}

type otlpAttribute struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

type otlpMetrics struct {
	ResourceMetrics []struct {
		Resource     any `json:"resource"`
		ScopeMetrics []struct {
			Scope   any `json:"scope"`
			Metrics []struct {
				Name string `json:"name"`
				Sum  struct {
					AggregationTemporality int   `json:"aggregationTemporality"`
					IsMonotonic            *bool `json:"isMonotonic"`
					DataPoints             []struct {
						Attributes []otlpAttribute `json:"attributes"`
						Start      json.RawMessage `json:"startTimeUnixNano"`
						End        json.RawMessage `json:"timeUnixNano"`
						AsInt      json.RawMessage `json:"asInt"`
						AsDouble   json.RawMessage `json:"asDouble"`
					} `json:"dataPoints"`
				} `json:"sum"`
			} `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

type otlpLogs struct {
	ResourceLogs []struct {
		Resource  any `json:"resource"`
		ScopeLogs []struct {
			Scope      any              `json:"scope"`
			LogRecords []map[string]any `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

// claudeVersion is the form of a Claude Code version we keep from `app.version`.
var claudeVersion = regexp.MustCompile(`^(unknown|[0-9]+\.[0-9]+\.[0-9]+)$`)

// modelName is the form of a model we keep from `model`, e.g.
// claude-sonnet-4-5-20250929, or a cloud provider's ID such as
// us.anthropic.claude-sonnet-4-5-20250929-v1:0 or claude-sonnet-4-5@20250929.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,99}$`)

// effortLevel is the form of an effort level we keep from `effort`: low,
// medium, high, xhigh or max today. A new level of the same form is kept too.
var effortLevel = regexp.MustCompile(`^[a-z]{1,16}$`)

// label is attrs[key] if it is a string of the given form, otherwise "".
func label(attrs map[string]any, key string, form *regexp.Regexp) string {
	v, _ := attrs[key].(string)
	if !form.MatchString(v) {
		return ""
	}
	return v
}

var tokenTypes = map[string]string{"input": "input", "output": "output", "cacheRead": "cacheRead", "cacheCreation": "cacheCreation"}

func fingerprint(v any) string {
	data, _ := json.Marshal(v) // maps are encoded with sorted keys
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// OTLP JSON allows 64-bit integers as strings or as numbers.
func rawInt(raw json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
	return n, err == nil
}
func rawFloat(raw json.RawMessage) (float64, bool) {
	n, err := strconv.ParseFloat(strings.Trim(string(raw), `"`), 64)
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}

// Typed AnyValue kinds are kept; other kinds are ignored.
func attributes(items []otlpAttribute) map[string]any {
	result := map[string]any{}
	for _, item := range items {
		for _, kind := range []string{"stringValue", "intValue", "doubleValue", "boolValue"} {
			if v, ok := item.Value[kind]; ok {
				result[item.Key] = v
				break
			}
		}
	}
	return result
}

// parseMetrics returns the recognized counter samples and the Claude Code
// version, if the payload carries a valid app.version attribute.
func parseMetrics(body []byte) ([]metricSample, string, error) {
	var payload otlpMetrics
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", invalid("invalid OTLP JSON")
	}
	var samples []metricSample
	version := ""
	for _, resource := range payload.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				if metric.Name != "claude_code.token.usage" && metric.Name != "claude_code.cost.usage" {
					continue
				}
				sum := metric.Sum
				if (sum.AggregationTemporality != 1 && sum.AggregationTemporality != 2) || sum.IsMonotonic == nil || !*sum.IsMonotonic {
					continue
				}
				for _, point := range sum.DataPoints {
					attrs := attributes(point.Attributes)
					if v, ok := attrs["app.version"].(string); ok && version == "" && claudeVersion.MatchString(v) {
						version = v
					}
					field := "cost"
					if metric.Name == "claude_code.token.usage" {
						typ, _ := attrs["type"].(string)
						if field = tokenTypes[typ]; field == "" {
							continue
						}
					}
					raw := point.AsInt
					if raw == nil {
						raw = point.AsDouble
					}
					amount, ok := rawFloat(raw)
					start, startOK := rawInt(point.Start)
					end, endOK := rawInt(point.End)
					if !ok || !startOK || !endOK || amount < 0 {
						continue
					}
					if field == "cost" && amount > 1_000_000 {
						continue
					}
					if field != "cost" && (amount != math.Trunc(amount) || amount > 1_000_000_000) {
						continue
					}
					series := fingerprint([]any{resource.Resource, scope.Scope, metric.Name, attrs, start})
					samples = append(samples, metricSample{
						Series: series, Key: fingerprint([]any{series, end, amount}), Field: field,
						Cumulative: sum.AggregationTemporality == 2, End: end, Amount: amount,
						Model: label(attrs, "model", modelName), Effort: label(attrs, "effort", effortLevel),
					})
				}
			}
		}
	}
	return samples, version, nil
}

var rateLimitText = regexp.MustCompile(`rate[ _-]?limit|usage limit|hit your limit|limit reached`)

// parseLogs returns one event per api_error record. The error text is read to
// classify rate limits and is not returned.
func parseLogs(body []byte) ([]logEvent, error) {
	var payload otlpLogs
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, invalid("invalid OTLP JSON")
	}
	var events []logEvent
	for _, resource := range payload.ResourceLogs {
		for _, scope := range resource.ScopeLogs {
			for _, record := range scope.LogRecords {
				var items []otlpAttribute
				data, _ := json.Marshal(record["attributes"])
				if json.Unmarshal(data, &items) != nil {
					continue
				}
				attrs := attributes(items)
				if name, _ := attrs["event.name"].(string); name != "api_error" && name != "claude_code.api_error" {
					continue
				}
				text, _ := attrs["error"].(string)
				code := fmt.Sprint(attrs["status_code"])
				events = append(events, logEvent{
					Key:       fingerprint([]any{resource.Resource, scope.Scope, record}),
					RateLimit: code == "429" || rateLimitText.MatchString(strings.ToLower(text)),
				})
			}
		}
	}
	return events, nil
}

type otlpTraces struct {
	ResourceSpans []struct {
		ScopeSpans []struct {
			Spans []struct {
				Name       string          `json:"name"`
				Attributes []otlpAttribute `json:"attributes"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// parseTraces returns the effort levels of the claude_code.llm_request spans.
// Nothing else is read from traces.
func parseTraces(body []byte) ([]string, error) {
	var payload otlpTraces
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, invalid("invalid OTLP JSON")
	}
	var efforts []string
	for _, resource := range payload.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				if span.Name != "claude_code.llm_request" {
					continue
				}
				if effort := label(attributes(span.Attributes), "effort", effortLevel); effort != "" {
					efforts = append(efforts, effort)
				}
			}
		}
	}
	return efforts, nil
}
