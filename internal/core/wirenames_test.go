package core

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The agent run keeps its old wire names ("source", "sources") on purpose.
func TestAgentRunWireNamesAreKept(t *testing.T) {
	b, err := json.Marshal(Report{AgentRun: "x"})
	if err != nil || !strings.Contains(string(b), `"source":"x"`) {
		t.Errorf("report json: %s %v", b, err)
	}
	b, err = json.Marshal(Run{AgentRuns: map[string]*AgentRunUsage{"x": {}}})
	if err != nil || !strings.Contains(string(b), `"sources":`) {
		t.Errorf("run json: %s %v", b, err)
	}
	var o Report
	d := json.NewDecoder(bytes.NewReader([]byte(`{"source":"x"}`)))
	d.DisallowUnknownFields()
	if err := d.Decode(&o); err != nil || o.AgentRun != "x" {
		t.Errorf("decode: %+v %v", o, err)
	}
}
