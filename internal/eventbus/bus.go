package eventbus

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Bus matches inbound events to subscriptions and drives a turn per match.
type Bus struct {
	driver SessionDriver
	dir    string // state dir for per-subscription persistence
	maxHop int

	mu   sync.Mutex
	subs []*registered
}

type registered struct {
	sub   Subscription
	store *subStore
}

// New builds a Bus. dir is where per-subscription state is persisted; maxHop
// bounds the agent-emitted event chain (loop guard); driver bridges to the agent
// runtime.
func New(driver SessionDriver, dir string, maxHop int) *Bus {
	if maxHop <= 0 {
		maxHop = 8
	}
	return &Bus{driver: driver, dir: dir, maxHop: maxHop}
}

// Register adds a subscription. Returns an error on a duplicate name or bad
// persistence.
func (b *Bus) Register(sub Subscription) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.subs {
		if r.sub.Name == sub.Name {
			return fmt.Errorf("eventbus: subscription %q already registered", sub.Name)
		}
	}
	store, err := newSubStore(b.dir, sub.Name, sub.Dedup)
	if err != nil {
		return err
	}
	b.subs = append(b.subs, &registered{sub: sub, store: store})
	return nil
}

// Unregister removes a subscription by name. Returns false if not found. The
// per-subscription dedup/binding file on disk is left in place, so re-registering
// the same name resumes its dedup window.
func (b *Bus) Unregister(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, r := range b.subs {
		if r.sub.Name == name {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			return true
		}
	}
	return false
}

// Handlers returns the names of the currently registered subscriptions.
func (b *Bus) Handlers() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.subs))
	for _, r := range b.subs {
		out = append(out, r.sub.Name)
	}
	return out
}

// Dispatch delivers one event to every matching subscription and returns a
// result per matched subscription. An event whose Hop exceeds maxHop is rejected
// (loop guard) and returns no results.
func (b *Bus) Dispatch(e Event) []DispatchResult {
	if e.Hop > b.maxHop {
		return nil
	}
	b.mu.Lock()
	regs := append([]*registered(nil), b.subs...)
	b.mu.Unlock()

	var results []DispatchResult
	for _, r := range regs {
		if r.sub.Match == nil || !r.sub.Match(e) {
			continue
		}
		if !r.store.markSeen(e.ID) {
			results = append(results, DispatchResult{Subscription: r.sub.Name, EventID: e.ID, Skipped: true})
			continue
		}
		sid, prompt, err := b.resolve(r, e)
		if err == nil && prompt != "" {
			err = b.driver.SendUserMessage(sid, prompt)
		}
		results = append(results, DispatchResult{Subscription: r.sub.Name, EventID: e.ID, SessionID: sid, Err: err})
	}
	return results
}

// resolve produces the (sessionID, prompt) for one matched subscription per its
// policy.
func (b *Bus) resolve(r *registered, e Event) (string, string, error) {
	switch p := r.sub.Policy.(type) {
	case Stateless:
		sid, err := b.driver.CreateSession(p.Agent, p.EnvID)
		if err != nil {
			return "", "", err
		}
		r.store.addCreated(sid)
		return sid, p.Prompt(e), nil

	case Keyed:
		key := p.Key(e)
		if sid, ok := r.store.binding(key); ok && b.alive(sid) {
			return sid, p.Prompt(e), nil
		}
		sid, err := b.driver.CreateSession(p.Agent, p.EnvID)
		if err != nil {
			return "", "", err
		}
		r.store.bind(key, sid)
		r.store.addCreated(sid)
		return sid, p.Prompt(e), nil

	case Routed:
		return b.resolveRouted(r, p, e)

	default:
		return "", "", fmt.Errorf("eventbus: unknown policy %T", r.sub.Policy)
	}
}

// alive reports whether a session exists and is not archived.
func (b *Bus) alive(sessionID string) bool {
	s, err := b.driver.SessionSummary(sessionID)
	return err == nil && !s.Archived
}

func (b *Bus) resolveRouted(r *registered, p Routed, e Event) (string, string, error) {
	max := p.Router.MaxCandidates
	if max <= 0 {
		max = defaultMaxCandidates
	}
	var cands []SessionSummary
	for _, sid := range r.store.recentCreated(max) {
		s, err := b.driver.SessionSummary(sid)
		if err != nil || s.Archived {
			continue
		}
		cands = append(cands, s)
	}

	dec := b.routerDecide(p, e, cands)
	prompt := dec.Prompt
	if prompt == "" && p.Prompt != nil {
		prompt = p.Prompt(e)
	}

	// Reuse only a candidate that is valid + alive; otherwise create new.
	if dec.ReuseSessionID != "" && candidateContains(cands, dec.ReuseSessionID) {
		return dec.ReuseSessionID, prompt, nil
	}
	sid, err := b.driver.CreateSession(p.Agent, p.EnvID)
	if err != nil {
		return "", "", err
	}
	r.store.addCreated(sid)
	return sid, prompt, nil
}

type routerDecision struct {
	ReuseSessionID string `json:"reuse_session_id"`
	Prompt         string `json:"prompt"`
}

// routerDecide runs the router agent and parses its JSON reply. Any failure
// degrades to an empty decision (→ new session), never an error.
func (b *Bus) routerDecide(p Routed, e Event, cands []SessionSummary) routerDecision {
	reply, err := b.driver.RunForReply(p.Router.Agent, p.EnvID, buildRouterPrompt(p, e, cands))
	if err != nil {
		return routerDecision{}
	}
	dec, ok := parseRouterReply(reply)
	if !ok {
		return routerDecision{}
	}
	return dec
}

func candidateContains(cands []SessionSummary, id string) bool {
	for _, c := range cands {
		if c.SessionID == id {
			return true
		}
	}
	return false
}

// buildRouterPrompt embeds the event + candidate summaries and asks for a strict
// JSON decision.
func buildRouterPrompt(p Routed, e Event, cands []SessionSummary) string {
	var sb strings.Builder
	if p.Router.SystemPrompt != "" {
		sb.WriteString(p.Router.SystemPrompt)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Incoming event:\n")
	ev, _ := json.MarshalIndent(map[string]any{
		"type": e.Type, "subject": e.Subject, "payload": e.Payload,
	}, "", "  ")
	sb.Write(ev)
	sb.WriteString("\n\nCandidate sessions (most recent first):\n")
	if len(cands) == 0 {
		sb.WriteString("(none)\n")
	}
	for _, c := range cands {
		fmt.Fprintf(&sb, "- session_id=%s seed=%q last=%q\n", c.SessionID, c.Seed, c.Last)
	}
	sb.WriteString("\nDecide whether this event belongs to an existing session. " +
		"Reply with ONLY a JSON object:\n" +
		`{"reuse_session_id": "<existing session_id or empty string>", "prompt": "<prompt for the handling agent>"}` + "\n")
	return sb.String()
}

// parseRouterReply extracts the first JSON object from the reply (tolerant of
// surrounding prose / code fences).
func parseRouterReply(reply string) (routerDecision, bool) {
	start := strings.IndexByte(reply, '{')
	end := strings.LastIndexByte(reply, '}')
	if start < 0 || end <= start {
		return routerDecision{}, false
	}
	var dec routerDecision
	if err := json.Unmarshal([]byte(reply[start:end+1]), &dec); err != nil {
		return routerDecision{}, false
	}
	return dec, true
}
