package core

import "testing"

func metricsPayload(name string, temporality int, monotonic string, attrs string, start, end, value string) []byte {
	return []byte(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},"scopeMetrics":[{"scope":{"name":"com.anthropic.claude_code"},"metrics":[{"name":"` + name +
		`","sum":{"aggregationTemporality":` + string(rune('0'+temporality)) + `,"isMonotonic":` + monotonic + `,"dataPoints":[{"attributes":[` + attrs + `],"startTimeUnixNano":"` + start + `","timeUnixNano":"` + end + `",` + value + `}]}}]}]}]}`)
}

const typeInput = `{"key":"type","value":{"stringValue":"input"}}`

func TestParseMetricsKeepsOnlyRecognizedCounters(t *testing.T) {
	samples, version, err := parseMetrics(metricsPayload("claude_code.token.usage", 1, "true",
		typeInput+`,{"key":"app.version","value":{"stringValue":"2.1.296"}},{"key":"user.email","value":{"stringValue":"a@b.c"}}`, "1", "2", `"asInt":"42"`))
	if err != nil || len(samples) != 1 || version != "2.1.296" {
		t.Fatalf("%v %v %q", samples, err, version)
	}
	if s := samples[0]; s.Field != "input" || s.Amount != 42 || s.Cumulative || s.End != 2 {
		t.Fatalf("wrong sample: %+v", s)
	}
	samples, _, _ = parseMetrics(metricsPayload("claude_code.cost.usage", 2, "true", ``, "1", "2", `"asDouble":0.0123`))
	if len(samples) != 1 || samples[0].Field != "cost" || !samples[0].Cumulative || samples[0].Amount != 0.0123 {
		t.Fatalf("wrong cost sample: %+v", samples)
	}
}

func TestParseMetricsIgnoresUnusableSamples(t *testing.T) {
	for name, body := range map[string][]byte{
		"other metric":     metricsPayload("claude_code.session.count", 1, "true", ``, "1", "2", `"asInt":"1"`),
		"unknown type":     metricsPayload("claude_code.token.usage", 1, "true", `{"key":"type","value":{"stringValue":"other"}}`, "1", "2", `"asInt":"1"`),
		"missing type":     metricsPayload("claude_code.token.usage", 1, "true", ``, "1", "2", `"asInt":"1"`),
		"not monotonic":    metricsPayload("claude_code.token.usage", 1, "false", typeInput, "1", "2", `"asInt":"1"`),
		"unknown temporal": metricsPayload("claude_code.token.usage", 3, "true", typeInput, "1", "2", `"asInt":"1"`),
		"negative":         metricsPayload("claude_code.token.usage", 1, "true", typeInput, "1", "2", `"asInt":"-1"`),
		"fractional token": metricsPayload("claude_code.token.usage", 1, "true", typeInput, "1", "2", `"asDouble":1.5`),
		"huge tokens":      metricsPayload("claude_code.token.usage", 1, "true", typeInput, "1", "2", `"asInt":"2000000000"`),
		"huge cost":        metricsPayload("claude_code.cost.usage", 1, "true", ``, "1", "2", `"asDouble":2000000`),
		"no value":         metricsPayload("claude_code.token.usage", 1, "true", typeInput, "1", "2", `"x":1`),
		"bad time":         metricsPayload("claude_code.token.usage", 1, "true", typeInput, "x", "2", `"asInt":"1"`),
	} {
		if samples, _, err := parseMetrics(body); err != nil || len(samples) != 0 {
			t.Errorf("%s: accepted %+v, %v", name, samples, err)
		}
	}
	if _, _, err := parseMetrics([]byte(`not json`)); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestParseMetricsIdentity(t *testing.T) {
	a, _, _ := parseMetrics(metricsPayload("claude_code.token.usage", 2, "true", typeInput, "1", "2", `"asInt":"5"`))
	b, _, _ := parseMetrics(metricsPayload("claude_code.token.usage", 2, "true", typeInput, "1", "3", `"asInt":"9"`))
	c, _, _ := parseMetrics(metricsPayload("claude_code.token.usage", 2, "true", typeInput, "7", "8", `"asInt":"9"`))
	again, _, _ := parseMetrics(metricsPayload("claude_code.token.usage", 2, "true", typeInput, "1", "2", `"asInt":"5"`))
	if a[0].Series != b[0].Series || a[0].Key == b[0].Key {
		t.Error("exports of one counter must share a series and differ in key")
	}
	if a[0].Series == c[0].Series {
		t.Error("a new start time is a new counter")
	}
	if a[0].Key != again[0].Key {
		t.Error("a repeated export must have the same key")
	}
}

func logsPayload(attrs string) []byte {
	return []byte(`{"resourceLogs":[{"resource":{},"scopeLogs":[{"scope":{},"logRecords":[{"timeUnixNano":"5","attributes":[` + attrs + `]}]}]}]}`)
}
func str(k, v string) string { return `{"key":"` + k + `","value":{"stringValue":"` + v + `"}}` }

func TestParseLogs(t *testing.T) {
	for name, c := range map[string]struct {
		attrs      string
		count      int
		rateLimits bool
	}{
		"plain error":     {str("event.name", "api_error") + `,` + str("error", "boom"), 1, false},
		"prefixed name":   {str("event.name", "claude_code.api_error"), 1, false},
		"http 429 string": {str("event.name", "api_error") + `,` + str("status_code", "429"), 1, true},
		"http 429 int":    {str("event.name", "api_error") + `,{"key":"status_code","value":{"intValue":"429"}}`, 1, true},
		"limit message":   {str("event.name", "api_error") + `,` + str("error", "You hit your limit"), 1, true},
		"request":         {str("event.name", "api_request"), 0, false},
		"no name":         {str("error", "x"), 0, false},
	} {
		events, err := parseLogs(logsPayload(c.attrs))
		if err != nil || len(events) != c.count || (c.count == 1 && events[0].RateLimit != c.rateLimits) {
			t.Errorf("%s: %+v %v", name, events, err)
		}
	}
	a, _ := parseLogs(logsPayload(str("event.name", "api_error")))
	b, _ := parseLogs(logsPayload(str("event.name", "api_error")))
	if a[0].Key != b[0].Key {
		t.Error("identical records must have the same key")
	}
	if _, err := parseLogs([]byte(`[`)); err == nil {
		t.Error("invalid JSON accepted")
	}
}
