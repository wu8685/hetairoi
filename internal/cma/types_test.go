package cma

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelConfig_UnmarshalBothForms(t *testing.T) {
	var bare ModelConfig
	if err := json.Unmarshal([]byte(`"claude-opus-4-8"`), &bare); err != nil {
		t.Fatalf("bare string: %v", err)
	}
	if bare.ID != "claude-opus-4-8" {
		t.Errorf("bare.ID = %q", bare.ID)
	}

	var obj ModelConfig
	if err := json.Unmarshal([]byte(`{"id":"claude-opus-4-8","speed":"fast"}`), &obj); err != nil {
		t.Fatalf("object: %v", err)
	}
	if obj.ID != "claude-opus-4-8" || obj.Speed != "fast" {
		t.Errorf("obj = %+v", obj)
	}
}

func TestModelConfig_MarshalAlwaysObject(t *testing.T) {
	b, err := json.Marshal(ModelConfig{ID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "{") {
		t.Errorf("model should marshal as object, got %s", b)
	}
}

func TestAgentRef_UnmarshalBothForms(t *testing.T) {
	var bare AgentRef
	if err := json.Unmarshal([]byte(`"agent_123"`), &bare); err != nil {
		t.Fatalf("bare: %v", err)
	}
	if bare.ID != "agent_123" || bare.Type != "agent" {
		t.Errorf("bare = %+v", bare)
	}

	var obj AgentRef
	if err := json.Unmarshal([]byte(`{"id":"agent_123","version":4}`), &obj); err != nil {
		t.Fatalf("obj: %v", err)
	}
	if obj.ID != "agent_123" || obj.Version != 4 || obj.Type != "agent" {
		t.Errorf("obj = %+v", obj)
	}
}

// List with no next_page must still serialize next_page (as null) and data.
func TestList_MarshalShape(t *testing.T) {
	b, err := json.Marshal(List[int]{Data: []int{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["data"]; !ok {
		t.Error("missing data field")
	}
	if string(m["next_page"]) != "null" {
		t.Errorf("next_page = %s, want null", m["next_page"])
	}
}

// Required list/map fields on an Agent must serialize as [] / {}, never null.
func TestAgent_RequiredFieldsNotNull(t *testing.T) {
	b, err := json.Marshal(Agent{ID: "agent_1", Tools: []ToolDef{}, Skills: []SkillRef{}, MCPServers: []MCPServer{}, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, frag := range []string{`"tools":[]`, `"skills":[]`, `"mcp_servers":[]`, `"metadata":{}`} {
		if !strings.Contains(s, frag) {
			t.Errorf("missing %s in %s", frag, s)
		}
	}
}
