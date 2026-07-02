// Package cma holds the Anthropic Managed Agents (CMA) wire types — the
// external API surface this service exposes so the official `anthropic` SDK
// (pointed at our base_url) can drive it as a drop-in.
//
// Shapes follow platform.claude.com/docs/en/managed-agents/* under the beta
// header `managed-agents-2026-04-01`. Where the public docs were silent on an
// exact field, the type is annotated with a TODO to verify against the live
// API before GA.
package cma

import (
	"encoding/json"
	"time"
)

// ----- shared building blocks -----

// ModelConfig is the `model` field on an agent. The API accepts either a bare
// string ("claude-opus-4-8") or an object ({id, speed}); we accept both.
type ModelConfig struct {
	ID    string `json:"id"`
	Speed string `json:"speed,omitempty"`
}

func (m *ModelConfig) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		m.ID = s
		return nil
	}
	type alias ModelConfig
	return json.Unmarshal(b, (*alias)(m))
}

// MarshalJSON always emits the object form for stability in responses.
func (m ModelConfig) MarshalJSON() ([]byte, error) {
	type alias ModelConfig
	return json.Marshal(alias(m))
}

// ToolDef covers all three tool kinds: prebuilt toolset
// (agent_toolset_20260401), mcp_toolset, and custom. Custom tools are accepted
// at the wire level but not yet executed (deferred — see project notes).
type ToolDef struct {
	Type          string          `json:"type"`
	Name          string          `json:"name,omitempty"`
	MCPServerName string          `json:"mcp_server_name,omitempty"`
	Description   string          `json:"description,omitempty"`
	InputSchema   json.RawMessage `json:"input_schema,omitempty"`
	DefaultConfig json.RawMessage `json:"default_config,omitempty"`
	Configs       json.RawMessage `json:"configs,omitempty"`
}

// SkillRef references a prebuilt ("anthropic") or "custom" skill.
type SkillRef struct {
	Type    string `json:"type"`
	SkillID string `json:"skill_id"`
	Version string `json:"version,omitempty"`
}

// MCPServer declares an MCP endpoint on the agent (auth lives in vaults; not
// yet wired).
type MCPServer struct {
	Type string `json:"type"` // "url"
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ContentBlock is one block of message content. MVP handles text only.
type ContentBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text,omitempty"`
}

// ----- Agent -----

// Agent is a persisted, versioned config. model/system/tools live here, never
// on a session.
type Agent struct {
	Type        string            `json:"type"` // "agent"
	ID          string            `json:"id"`
	Version     int64             `json:"version"`
	Name        string            `json:"name"`
	Model       ModelConfig       `json:"model"`
	System      string            `json:"system,omitempty"`
	Description string            `json:"description,omitempty"`
	// These four are REQUIRED in the SDK response model — always emit [] / {},
	// never null or absent (no omitempty; handlers normalize nil -> empty).
	Tools      []ToolDef         `json:"tools"`
	Skills     []SkillRef        `json:"skills"`
	MCPServers []MCPServer       `json:"mcp_servers"`
	Metadata   map[string]string `json:"metadata"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	ArchivedAt *time.Time        `json:"archived_at"`
}

// AgentCreateRequest is the POST /v1/agents body. Same as POST /v1/agents/{id}
// (update) which produces a new version.
type AgentCreateRequest struct {
	Name        string            `json:"name"`
	Model       ModelConfig       `json:"model"`
	System      string            `json:"system,omitempty"`
	Description string            `json:"description,omitempty"`
	Tools       []ToolDef         `json:"tools,omitempty"`
	Skills      []SkillRef        `json:"skills,omitempty"`
	MCPServers  []MCPServer       `json:"mcp_servers,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// ----- Environment -----

// Environment is, in this service, a logical resource: we synthesize an
// env_id and rely on ahsir's per-agent workspace + filesystem allow-list for
// isolation rather than provisioning a real container.
type Environment struct {
	Type        string            `json:"type"` // "environment"
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Config      json.RawMessage   `json:"config,omitempty"`
	Metadata    map[string]string `json:"metadata"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ArchivedAt  *time.Time        `json:"archived_at"`
}

// EnvironmentCreateRequest is the POST /v1/environments body.
type EnvironmentCreateRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Config      json.RawMessage   `json:"config,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// EnvironmentUpdateRequest is the POST /v1/environments/{id} body. Pointer /
// nilable fields give partial-update semantics: only fields present in the
// request are applied.
type EnvironmentUpdateRequest struct {
	Name        *string           `json:"name"`
	Description *string           `json:"description"`
	Config      json.RawMessage   `json:"config"`
	Metadata    map[string]string `json:"metadata"`
}

// ----- Session -----

// AgentRef is the `agent` field on a session: a bare ID (latest version) or
// {type:"agent", id, version}.
type AgentRef struct {
	Type    string `json:"type,omitempty"`
	ID      string `json:"id"`
	Version int64  `json:"version,omitempty"`
}

func (r *AgentRef) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		r.Type = "agent"
		r.ID = s
		return nil
	}
	type alias AgentRef
	a := (*alias)(r)
	if err := json.Unmarshal(b, a); err != nil {
		return err
	}
	if r.Type == "" {
		r.Type = "agent"
	}
	return nil
}

// Session statuses.
const (
	StatusRescheduling = "rescheduling"
	StatusRunning      = "running"
	StatusIdle         = "idle"
	StatusTerminated   = "terminated"
)

// SessionAgent is the resolved agent snapshot embedded in a session response.
// The SDK requires the full object here (not a bare ref): id, name, model,
// version, type, and the required-list fields.
type SessionAgent struct {
	Type        string      `json:"type"` // "agent"
	ID          string      `json:"id"`
	Version     int64       `json:"version"`
	Name        string      `json:"name"`
	Model       ModelConfig `json:"model"`
	System      string      `json:"system,omitempty"`
	Description string      `json:"description,omitempty"`
	Tools       []ToolDef   `json:"tools"`
	Skills      []SkillRef  `json:"skills"`
	MCPServers  []MCPServer `json:"mcp_servers"`
}

// SessionStats / SessionUsage are required objects in the SDK model; their
// fields are all optional, so an empty object validates.
type SessionStats struct{}
type SessionUsage struct{}

// Session is a stateful interaction with an agent.
type Session struct {
	Type          string            `json:"type"` // "session"
	ID            string            `json:"id"`
	Title         string            `json:"title,omitempty"`
	Status        string            `json:"status"`
	EnvironmentID string            `json:"environment_id"`
	Agent         SessionAgent      `json:"agent"`
	Resources     []any             `json:"resources"`
	VaultIDs      []string          `json:"vault_ids"`
	Stats         SessionStats      `json:"stats"`
	Usage         SessionUsage      `json:"usage"`
	Metadata      map[string]string `json:"metadata"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	ArchivedAt    *time.Time        `json:"archived_at"`
}

// SessionCreateRequest is the POST /v1/sessions body.
type SessionCreateRequest struct {
	Agent         AgentRef          `json:"agent"`
	EnvironmentID string            `json:"environment_id"`
	Title         string            `json:"title,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	// resources / vault_ids accepted but ignored in MVP.
	Resources json.RawMessage `json:"resources,omitempty"`
	VaultIDs  []string        `json:"vault_ids,omitempty"`
}

// SessionUpdateRequest is the POST /v1/sessions/{id} body. Pointer / nilable
// fields give partial-update semantics.
type SessionUpdateRequest struct {
	Title    *string           `json:"title"`
	Metadata map[string]string `json:"metadata"`
	VaultIDs []string          `json:"vault_ids"`
}

// DeletedResource is the body returned by DELETE on environments / sessions:
// {id, type:"<resource>_deleted"}.
type DeletedResource struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// ----- Events -----

// Inbound event types (client → session).
const (
	EvtUserMessage          = "user.message"
	EvtUserInterrupt        = "user.interrupt"
	EvtUserToolConfirmation = "user.tool_confirmation"
	EvtUserCustomToolResult = "user.custom_tool_result"
)

// Outbound event types (session → client) emitted on the stream.
const (
	EvtAgentMessage           = "agent.message"
	EvtAgentThinking          = "agent.thinking"
	EvtAgentToolUse           = "agent.tool_use"
	EvtAgentMCPToolUse        = "agent.mcp_tool_use"
	EvtAgentToolResult        = "agent.tool_result"
	EvtAgentMCPToolResult     = "agent.mcp_tool_result"
	EvtSpanModelRequestStart  = "span.model_request_start"
	EvtSpanModelRequestEnd    = "span.model_request_end"
	EvtSessionStatusRunning     = "session.status_running"
	EvtSessionStatusIdle        = "session.status_idle"
	EvtSessionStatusTerminate   = "session.status_terminated"
	EvtSessionStatusRescheduled = "session.status_rescheduled"
	EvtSessionError             = "session.error"
	EvtSessionDeleted           = "session.deleted"
)

// StopReason rides on session.status_idle. type ∈ end_turn | requires_action |
// retries_exhausted.
type StopReason struct {
	Type     string   `json:"type"`
	EventIDs []string `json:"event_ids,omitempty"`
}

// RetryStatus is the discriminated retry state on a session error.
// type ∈ retrying | exhausted | terminal.
type RetryStatus struct {
	Type string `json:"type"`
}

// EventError rides on session.error. `type` must be one of the SDK's error
// codes (unknown_error is the safe fallback); retry_status is required.
type EventError struct {
	Type        string      `json:"type"`
	Message     string      `json:"message"`
	RetryStatus RetryStatus `json:"retry_status"`
}

// ModelUsage rides on span.model_request_end. All token counts are required
// ints in the SDK model (zero is valid); speed is optional.
type ModelUsage struct {
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	Speed                    string `json:"speed,omitempty"`
}

// Event is one event on the stream / in the event list. Type-specific fields
// are omitempty so a single struct serializes cleanly for every type.
type Event struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	ProcessedAt *time.Time     `json:"processed_at"`
	Content     []ContentBlock `json:"content,omitempty"`
	StopReason  *StopReason    `json:"stop_reason,omitempty"`
	Error       *EventError    `json:"error,omitempty"`

	// agent.tool_use / agent.mcp_tool_use: the tool invocation.
	Name          string          `json:"name,omitempty"`
	Input         json.RawMessage `json:"input,omitempty"`
	MCPServerName string          `json:"mcp_server_name,omitempty"`

	// agent.tool_result / agent.mcp_tool_result: the tool outcome. content
	// reuses the Content field above; the *_tool_use_id links to the call.
	ToolUseID    string `json:"tool_use_id,omitempty"`
	MCPToolUseID string `json:"mcp_tool_use_id,omitempty"`
	IsError      bool   `json:"is_error,omitempty"`

	// span.model_request_end: links back to the start span + usage.
	ModelRequestStartID string      `json:"model_request_start_id,omitempty"`
	ModelUsage          *ModelUsage `json:"model_usage,omitempty"`
}

// SendEventsRequest is the POST /v1/sessions/{id}/events body.
type SendEventsRequest struct {
	Events []Event `json:"events"`
}

// List is the cursor-page envelope the SDK's SyncPageCursor parses: `data`
// (required) plus an optional `next_page` cursor.
type List[T any] struct {
	Data     []T     `json:"data"`
	NextPage *string `json:"next_page"`
}

// APIError is the standard Anthropic error envelope.
type APIError struct {
	Type      string `json:"type"` // "error"
	Error     Err    `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

type Err struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
