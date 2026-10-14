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
