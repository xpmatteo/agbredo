package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// The agent run keeps its old wire name ("sources") on purpose.
func TestAgentRunWireNamesAreKept(t *testing.T) {
	b, err := json.Marshal(Run{AgentRuns: map[string]*AgentRunUsage{"x": {}}})
	if err != nil || !strings.Contains(string(b), `"sources":`) {
		t.Errorf("run json: %s %v", b, err)
	}
}

// Run IDs of stored runs must not change: the ID still hashes the "observe:" prefix.
func TestRunIDsStayStable(t *testing.T) {
	if got := attemptRunID("agent", 777, 1); got != "74e0bd848e65945b5f45ee6ffe2a08ed4c5ecd064f813dc346ecb97ec4b05cee" {
		t.Errorf("run ID changed: %s", got)
	}
}
