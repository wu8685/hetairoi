// Package translate maps between the CMA wire model and ahsir's agent card.
package translate

import (
	"fmt"
	"strings"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/cma"
)

// RuntimeDefaults carry the provider credentials baked into every card.
type RuntimeDefaults struct {
	Provider string
	BaseURL  string
	APIKey   string
}

// AhsirAgentName derives a stable, unique ahsir agent name for one
// (agentID, version). Versioning lives entirely here — ahsir sees distinct
// agents and stays version-agnostic.
//
// agentID looks like "agent_<base32>"; we drop the prefix and append the
// version so the name is a clean DNS-ish token.
func AhsirAgentName(agentID string, version int64) string {
	id := strings.TrimPrefix(agentID, cma.PrefixAgent+"_")
	return fmt.Sprintf("cma-%s-v%d", id, version)
}

// AgentToCard renders a versioned CMA agent into an ahsir card.
//
//   - model        -> runtime.model (provider/baseURL/apiKey from defaults)
//   - system       -> claude.systemPrompt
//   - skills        -> descriptive skills list (ahsir skills are descriptive)
//   - mcp_servers  -> mcp.servers (claude remote-server shape; auth deferred)
//   - filesystem    -> enabled + write_access so the prebuilt toolset
//     (bash/read/write/edit) is available, the agent_toolset_20260401 analog
//   - streaming.partial_messages -> true so deltas flow once A2A is wired
func AgentToCard(name string, a *cma.Agent, d RuntimeDefaults) *ahsir.AgentCard {
	card := &ahsir.AgentCard{
		Name:        name,
		Description: a.Description,
		Version:     fmt.Sprintf("%d", a.Version),
		Claude: ahsir.ClaudeConfig{
			SystemPrompt: a.System,
		},
		Runtime: ahsir.RuntimeConfig{
			Provider: d.Provider,
			BaseURL:  d.BaseURL,
			APIKey:   d.APIKey,
			Model:    a.Model.ID,
			// Optional per-agent override of ahsir's 120s runtime timeout, for
			// long-running tool-driven turns (e.g. an event agent that shells
			// out repeatedly). Empty -> ahsir default.
			Timeout: a.Metadata["runtime_timeout"],
		},
		Filesystem: ahsir.FilesystemConfig{
			Enabled:     true,
			WriteAccess: true,
			// Opt into the Bash tool only when the agent explicitly asks for it
			// via metadata — e.g. an event-driven agent that must run git/CLI
			// tools itself. Default stays shell-less.
			ShellAccess:  a.Metadata["shell_access"] == "true",
			AllowedPaths: []string{"."},
		},
		Streaming: ahsir.StreamingConfig{PartialMessages: true},
	}

	for _, s := range a.Skills {
		card.Skills = append(card.Skills, ahsir.SkillConfig{
			Name:        s.SkillID,
			Description: skillDescription(s),
		})
	}

	if len(a.MCPServers) > 0 {
		servers := map[string]any{}
		for _, m := range a.MCPServers {
			// claude remote MCP shape; vault-based auth is deferred.
			servers[m.Name] = map[string]any{"type": "http", "url": m.URL}
		}
		card.MCP = ahsir.MCPConfig{Servers: servers}
	}

	return card
}

func skillDescription(s cma.SkillRef) string {
	if s.Type == "anthropic" {
		return "anthropic prebuilt skill: " + s.SkillID
	}
	return "custom skill: " + s.SkillID
}
