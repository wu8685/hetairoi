package translate

import (
	"testing"

	"github.com/wu8685/hetairoi/internal/cma"
)

func TestAhsirAgentName(t *testing.T) {
	cases := []struct {
		id      string
		version int64
		want    string
	}{
		{"agent_abc123", 1, "cma-abc123-v1"},
		{"agent_abc123", 7, "cma-abc123-v7"},
		{"agent_", 2, "cma--v2"}, // degenerate but stable
	}
	for _, c := range cases {
		if got := AhsirAgentName(c.id, c.version); got != c.want {
			t.Errorf("AhsirAgentName(%q,%d) = %q, want %q", c.id, c.version, got, c.want)
		}
	}
}

func TestAgentToCard_Core(t *testing.T) {
	a := &cma.Agent{
		ID: "agent_x", Version: 3, Name: "researcher",
		Model:       cma.ModelConfig{ID: "claude-opus-4-8"},
		System:      "be concise",
		Description: "a researcher",
	}
	d := RuntimeDefaults{Provider: "anthropic", BaseURL: "https://api", APIKey: "sk-123"}
	card := AgentToCard("cma-x-v3", a, d)

	if card.Name != "cma-x-v3" {
		t.Errorf("name = %q", card.Name)
	}
	if card.Version != "3" {
		t.Errorf("version = %q, want \"3\"", card.Version)
	}
	if card.Claude.SystemPrompt != "be concise" {
		t.Errorf("systemPrompt = %q", card.Claude.SystemPrompt)
	}
	if card.Runtime.Model != "claude-opus-4-8" {
		t.Errorf("runtime.model = %q", card.Runtime.Model)
	}
	if card.Runtime.Provider != "anthropic" || card.Runtime.BaseURL != "https://api" || card.Runtime.APIKey != "sk-123" {
		t.Errorf("runtime creds not from defaults: %+v", card.Runtime)
	}
	// filesystem must be enabled+writable so the prebuilt toolset is available
	if !card.Filesystem.Enabled || !card.Filesystem.WriteAccess {
		t.Errorf("filesystem = %+v, want enabled+write", card.Filesystem)
	}
	// streaming flag must be set so deltas can flow once A2A is wired
	if !card.Streaming.PartialMessages {
		t.Error("streaming.partial_messages should be true")
	}
}

func TestAgentToCard_SkillsAndMCP(t *testing.T) {
	a := &cma.Agent{
		ID: "agent_y", Version: 1, Name: "y",
		Model: cma.ModelConfig{ID: "m"},
		Skills: []cma.SkillRef{
			{Type: "anthropic", SkillID: "pdf"},
			{Type: "custom", SkillID: "my-skill"},
		},
		MCPServers: []cma.MCPServer{
			{Type: "url", Name: "search", URL: "https://mcp.example/sse"},
		},
	}
	card := AgentToCard("cma-y-v1", a, RuntimeDefaults{})

	if len(card.Skills) != 2 {
		t.Fatalf("skills len = %d, want 2", len(card.Skills))
	}
	if card.Skills[0].Name != "pdf" || card.Skills[1].Name != "my-skill" {
		t.Errorf("skill names = %v", card.Skills)
	}
	srv, ok := card.MCP.Servers["search"].(map[string]any)
	if !ok {
		t.Fatalf("mcp server 'search' missing or wrong shape: %#v", card.MCP.Servers)
	}
	if srv["type"] != "http" || srv["url"] != "https://mcp.example/sse" {
		t.Errorf("mcp server = %v", srv)
	}
}

func TestAgentToCard_NoMCP(t *testing.T) {
	a := &cma.Agent{ID: "agent_z", Version: 1, Model: cma.ModelConfig{ID: "m"}}
	card := AgentToCard("cma-z-v1", a, RuntimeDefaults{})
	if card.MCP.Servers != nil {
		t.Errorf("expected nil MCP servers when agent has none, got %v", card.MCP.Servers)
	}
}
