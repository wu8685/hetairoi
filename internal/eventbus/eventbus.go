// Package eventbus turns inbound events into agent turns. A user registers
// handlers (subscriptions) that match event types; a matching event resolves a
// session (create or reuse, per policy), is shaped into a prompt, and drives
// that session for one turn. See docs/EVENTBUS-SPEC.md.
package eventbus

import (
	"encoding/json"
	"time"
)

// AgentRef identifies which agent (and optional pinned version) a handler uses.
type AgentRef struct {
	ID      string
	Version int64 // 0 = latest
}

// Event is one inbound event.
type Event struct {
	ID      string          // dedup key (required)
	Type    string          // matched against subscriptions
	Subject string          // routing key (incident-id / thread-id / user-id / ...)
	Payload json.RawMessage // raw event body
	Source  string          // origin, for audit
	Time    time.Time
	Hop     int    // loop guard — agent-emitted events increment this
	CauseID string // id of the event that caused this one (causal chain)
}

// SessionSummary is a compact, log-derived view of a session, used by the Routed
// policy's router to decide reuse.
type SessionSummary struct {
	SessionID    string
	CreatedAt    time.Time
	LastActiveAt time.Time
	Seed         string // first user.message, truncated — the session's topic seed
	Last         string // most recent agent.message, truncated — current state
	Archived     bool
}

// SessionDriver is how the bus drives the agent runtime (hetairoi). The bus
// depends only on this interface, so it is unit-tested against a fake.
type SessionDriver interface {
	CreateSession(agent AgentRef, envID string) (sessionID string, err error)
	SendUserMessage(sessionID, prompt string) error
	// RunForReply drives a one-shot stateless turn and returns the agent's final
	// text — used for the Routed router decision.
	RunForReply(agent AgentRef, envID, prompt string) (reply string, err error)
	SessionSummary(sessionID string) (SessionSummary, error)
}

// Policy is one of Stateless | Keyed | Routed.
type Policy interface{ isPolicy() }

// Stateless creates a fresh session for every event.
type Stateless struct {
	Agent  AgentRef
	EnvID  string
	Prompt func(Event) string
}

func (Stateless) isPolicy() {}

// Keyed reuses one session per deterministic key (no LLM).
type Keyed struct {
	Agent  AgentRef
	EnvID  string
	Key    func(Event) string
	Prompt func(Event) string
}

func (Keyed) isPolicy() {}

// Routed lets a router agent decide reuse among candidate sessions (LLM).
type Routed struct {
	Agent  AgentRef // handling agent (does the work)
	EnvID  string
	Router RouterSpec
	Prompt func(Event) string // fallback prompt if the router yields none
}

func (Routed) isPolicy() {}

// RouterSpec configures the Routed policy's routing agent.
type RouterSpec struct {
	Agent         AgentRef
	SystemPrompt  string
	MaxCandidates int // recency cap on candidates (default 20)
}

// DedupConfig is a subscription's per-handler dedup window.
type DedupConfig struct {
	MaxEntries int           // window by count (0 = default 1024)
	TTL        time.Duration // window by age (0 = no TTL)
}

// Subscription is one registered handler.
type Subscription struct {
	Name   string // identity; also the persistence namespace
	Match  func(Event) bool
	Policy Policy
	Dedup  DedupConfig
}

// DispatchResult records the outcome of delivering one event to one subscription.
type DispatchResult struct {
	Subscription string
	EventID      string
	SessionID    string // the resolved session (addressable for human follow-up)
	Skipped      bool   // true if deduped (already seen)
	Err          error
}

// MarshalJSON renders Err as a string field (a bare error marshals to {}).
func (r DispatchResult) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"subscription": r.Subscription,
		"event_id":     r.EventID,
		"session_id":   r.SessionID,
		"skipped":      r.Skipped,
	}
	if r.Err != nil {
		out["error"] = r.Err.Error()
	}
	return json.Marshal(out)
}

const defaultMaxCandidates = 20
const defaultDedupMaxEntries = 1024
