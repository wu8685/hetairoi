// Package ahsir is the client to the ahsir scheduler gateway — the internal
// agent runtime that backs every CMA session.
package ahsir

// AgentCard mirrors ahsir's internal wrapper.AgentCardConfig, restricted to the
// fields this service sets, with JSON tags equal to ahsir's YAML tags.
//
// This is the body of the proposed inline-registration admin endpoint
// (`POST /admin/agents` with a `card` field). For it to work end-to-end, ahsir
// must add matching `json` tags to AgentCardConfig (today it only has `yaml`
// tags) — that is the one agreed ahsir-side change, pending implementation.
type AgentCard struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Version     string           `json:"version,omitempty"`
	Skills      []SkillConfig    `json:"skills,omitempty"`
	Claude      ClaudeConfig     `json:"claude"`
	Runtime     RuntimeConfig    `json:"runtime"`
	Filesystem  FilesystemConfig `json:"filesystem"`
	Streaming   StreamingConfig  `json:"streaming"`
	MCP         MCPConfig        `json:"mcp,omitempty"`
}

type SkillConfig struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type ClaudeConfig struct {
	SystemPrompt  string `json:"systemPrompt,omitempty"`
	MaxAgentCalls int    `json:"maxAgentCalls,omitempty"`
}

type RuntimeConfig struct {
	Provider string            `json:"provider,omitempty"`
	BaseURL  string            `json:"baseURL,omitempty"`
	APIKey   string            `json:"apiKey,omitempty"`
	Model    string            `json:"model,omitempty"`
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Timeout  string            `json:"timeout,omitempty"`
}

type FilesystemConfig struct {
	Enabled      bool     `json:"enabled"`
	WriteAccess  bool     `json:"write_access"`
	// ShellAccess opts the agent into the Bash tool (arbitrary command
	// execution) — separate from WriteAccess by design. Maps to ahsir's
	// filesystem.shell_access card knob.
	ShellAccess  bool     `json:"shell_access"`
	AllowedPaths []string `json:"allowed_paths,omitempty"`
}

type StreamingConfig struct {
	PartialMessages bool `json:"partial_messages"`
}

type MCPConfig struct {
	Servers map[string]any `json:"servers,omitempty"`
}
