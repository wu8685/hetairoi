package eventbus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"text/template"
	"time"
)

// SourceSpec is the JSON-serializable declaration of an event source — the
// control-plane body for POST /v1/eventbus/sources. It compiles to a running
// Source via the Registry. Only the fields for the declared Type are read.
type SourceSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`               // "codehub-pr"
	Interval string `json:"interval,omitempty"` // Go duration; default 30s

	// CodeHubPR fields (Type == "codehub-pr").
	Project     string `json:"project,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	Author      string `json:"author,omitempty"`
	State       string `json:"state,omitempty"`
	EventType   string `json:"event_type,omitempty"` // emitted Event.Type (default "pr"/"workitem")
	AllowIIDs   []int  `json:"allow_iids,omitempty"`
	MergeStatus bool   `json:"merge_status,omitempty"` // fetch + key events by merge_status
	Bin         string `json:"bin,omitempty"`

	// WorkItem fields (Type == "workitem"). Project is reused for -p.
	Space        string   `json:"space,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	Belong       string   `json:"belong,omitempty"`
	StatusList   []string `json:"status_list,omitempty"`
	PageSize     int      `json:"page_size,omitempty"`
	IDField      string   `json:"id_field,omitempty"`
	VersionField string   `json:"version_field,omitempty"`

	// GitHub fields (Type == "github"). State is reused (open|closed|all).
	Repo            string `json:"repo,omitempty"`              // "owner/name"
	Kinds           string `json:"kinds,omitempty"`             // both | issue | pr
	AllowNumbers    []int  `json:"allow_numbers,omitempty"`     // blast-radius guard
	TokenFile       string `json:"token_file,omitempty"`        // path to the PAT file (secret stays out of the spec)
	APIBase         string `json:"api_base,omitempty"`          // default https://api.github.com
	IssueEventType  string `json:"issue_event_type,omitempty"`  // emitted type for issues (default "issue")
	PushEventType   string `json:"push_event_type,omitempty"`   // emitted type for PR commits (default "pr.push")
	ReviewEventType string `json:"review_event_type,omitempty"` // emitted type for PR reviews (default "pr.review")
	BuildLabel      string `json:"build_label,omitempty"`       // label opting an issue into the loop (default "agent-build")
	AgentPrefix     string `json:"agent_prefix,omitempty"`      // head-branch prefix marking a loop PR (default "agent/")
	BotMarker       string `json:"bot_marker,omitempty"`        // issue-comment self-trigger marker (default "<!-- cma-agent -->")
}

// HandlerSpec is the JSON-serializable declaration of a subscription — the body
// for POST /v1/eventbus/handlers. match/key/prompt are data (matcher struct +
// Go templates) so a handler can be created at runtime without compiled-in
// closures.
type HandlerSpec struct {
	Name   string      `json:"name"`
	Match  MatchSpec   `json:"match"`
	Policy PolicySpec  `json:"policy"`
	Dedup  DedupConfig `json:"dedup,omitempty"`
}

// MatchSpec is a declarative event predicate. An empty MatchSpec matches every
// event. Conditions are ANDed.
type MatchSpec struct {
	Type          string            `json:"type,omitempty"`           // exact Event.Type
	SubjectGlob   string            `json:"subject_glob,omitempty"`   // path.Match glob over Event.Subject
	PayloadEquals map[string]string `json:"payload_equals,omitempty"` // dotted path in payload -> expected string
}

// PolicySpec declares one of the three policies plus the templates that replace
// the Keyed/Routed closures.
type PolicySpec struct {
	Kind    string `json:"kind"` // "stateless" | "keyed" | "routed"
	AgentID string `json:"agent_id"`
	Version int64  `json:"version,omitempty"`
	EnvID   string `json:"env_id,omitempty"`

	// PromptTemplate renders the per-turn user message from the event (all
	// policies). KeyTemplate renders the reuse key (keyed only). Both are Go
	// text/templates over the event view (.type .subject .source .id .payload).
	PromptTemplate string `json:"prompt_template,omitempty"`
	KeyTemplate    string `json:"key_template,omitempty"`

	// Router configures the routed policy.
	Router *RouterSpecJSON `json:"router,omitempty"`
}

// RouterSpecJSON is the JSON form of RouterSpec.
type RouterSpecJSON struct {
	AgentID       string `json:"agent_id"`
	Version       int64  `json:"version,omitempty"`
	SystemPrompt  string `json:"system_prompt,omitempty"`
	MaxCandidates int    `json:"max_candidates,omitempty"`
}

// compileMatch turns a MatchSpec into a Match predicate. An invalid glob is a
// compile error so a bad handler is rejected at create time, not silently.
func (m MatchSpec) compile() (func(Event) bool, error) {
	if m.SubjectGlob != "" {
		if _, err := path.Match(m.SubjectGlob, ""); err != nil {
			return nil, fmt.Errorf("subject_glob %q: %w", m.SubjectGlob, err)
		}
	}
	return func(e Event) bool {
		if m.Type != "" && e.Type != m.Type {
			return false
		}
		if m.SubjectGlob != "" {
			ok, _ := path.Match(m.SubjectGlob, e.Subject)
			if !ok {
				return false
			}
		}
		if len(m.PayloadEquals) > 0 {
			view := decodePayload(e.Payload)
			for dotted, want := range m.PayloadEquals {
				if got, ok := lookupPath(view, dotted); !ok || stringify(got) != want {
					return false
				}
			}
		}
		return true
	}, nil
}

// compileToSubscription builds a Subscription from the spec. agent/env/templates
// are validated and compiled up front.
func (h HandlerSpec) compileToSubscription() (Subscription, error) {
	if h.Name == "" {
		return Subscription{}, fmt.Errorf("handler name is required")
	}
	match, err := h.Match.compile()
	if err != nil {
		return Subscription{}, err
	}
	if h.Policy.AgentID == "" {
		return Subscription{}, fmt.Errorf("policy.agent_id is required")
	}
	prompt, err := compileTemplate("prompt_template", h.Policy.PromptTemplate)
	if err != nil {
		return Subscription{}, err
	}
	agent := AgentRef{ID: h.Policy.AgentID, Version: h.Policy.Version}

	var policy Policy
	switch h.Policy.Kind {
	case "stateless", "":
		policy = Stateless{Agent: agent, EnvID: h.Policy.EnvID, Prompt: prompt}
	case "keyed":
		if h.Policy.KeyTemplate == "" {
			return Subscription{}, fmt.Errorf("keyed policy requires key_template")
		}
		key, err := compileTemplate("key_template", h.Policy.KeyTemplate)
		if err != nil {
			return Subscription{}, err
		}
		policy = Keyed{Agent: agent, EnvID: h.Policy.EnvID, Key: key, Prompt: prompt}
	case "routed":
		if h.Policy.Router == nil {
			return Subscription{}, fmt.Errorf("routed policy requires router")
		}
		policy = Routed{
			Agent: agent,
			EnvID: h.Policy.EnvID,
			Router: RouterSpec{
				Agent:         AgentRef{ID: h.Policy.Router.AgentID, Version: h.Policy.Router.Version},
				SystemPrompt:  h.Policy.Router.SystemPrompt,
				MaxCandidates: h.Policy.Router.MaxCandidates,
			},
			Prompt: prompt,
		}
	default:
		return Subscription{}, fmt.Errorf("unknown policy kind %q", h.Policy.Kind)
	}

	return Subscription{Name: h.Name, Match: match, Policy: policy, Dedup: h.Dedup}, nil
}

// compileTemplate parses a Go text/template that renders against the event view.
// An empty template yields a function returning "" (e.g. a stateless handler may
// want no prompt). The returned func never panics: a render error degrades to "".
func compileTemplate(name, src string) (func(Event) string, error) {
	if src == "" {
		return func(Event) string { return "" }, nil
	}
	t, err := template.New(name).Option("missingkey=zero").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return func(e Event) string {
		var buf bytes.Buffer
		if err := t.Execute(&buf, eventView(e)); err != nil {
			return ""
		}
		return strings.TrimSpace(buf.String())
	}, nil
}

// eventView is the data model templates and payload matches see.
func eventView(e Event) map[string]any {
	return map[string]any{
		"id":       e.ID,
		"type":     e.Type,
		"subject":  e.Subject,
		"source":   e.Source,
		"hop":      e.Hop,
		"cause_id": e.CauseID,
		"payload":  decodePayload(e.Payload),
	}
}

// decodePayload returns the payload as a map (object), or the raw string under
// no key if it is not a JSON object.
func decodePayload(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		return m
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		return v
	}
	return string(raw)
}

// lookupPath walks a dotted path over a decoded payload map.
func lookupPath(v any, dotted string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON numbers decode to float64; render integers without a trailing .0.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func parseInterval(s string) time.Duration {
	if s == "" {
		return 30 * time.Second
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return 30 * time.Second
}
