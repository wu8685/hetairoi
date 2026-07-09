package eventbus

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Retry errors, distinguished so the control plane can map them to HTTP codes.
var (
	// ErrHandlerNotFound is returned when no handler with the given name is
	// registered.
	ErrHandlerNotFound = errors.New("eventbus: handler not found")
	// ErrEventNotFound is returned when no stored event matches the retry target.
	ErrEventNotFound = errors.New("eventbus: no stored event for retry target")
	// ErrNoRetryTarget is returned when a retry request selects nothing.
	ErrNoRetryTarget = errors.New("eventbus: retry requires key, event_id, or subject")
)

// RetryTarget selects which previously-seen event to replay for a handler. The
// selector precedence is EventID → Key → Subject (the first non-empty wins). When
// FreshSession is set, a Keyed handler's binding is reset so a brand-new session
// is created instead of reusing the bound one.
type RetryTarget struct {
	Key          string
	EventID      string
	Subject      string
	FreshSession bool
}

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
		// Remember the payload so an operator can replay this event later
		// (native event-retry) without mutating upstream state.
		r.store.recordEvent(policyKey(r.sub, e), e)
		sid, prompt, reused, err := b.resolve(r, e)
		if err == nil && prompt != "" {
			err = b.driver.SendUserMessage(sid, prompt)
			// A reused session may point at an agent whose inline registration was
			// orphaned (e.g. a scheduler restart) → the turn 404s. Recover once by
			// forcing a fresh, re-registered session and retrying the turn.
			if err != nil && reused {
				if newSID, rerr := b.refresh(r, e); rerr == nil && newSID != "" {
					sid = newSID
					err = b.driver.SendUserMessage(sid, prompt)
				}
			}
		}
		results = append(results, DispatchResult{Subscription: r.sub.Name, EventID: e.ID, SessionID: sid, Err: err})
	}
	return results
}

// Retry replays a previously-seen event for one handler, bypassing dedup, with
// no change to upstream (GitHub/CodeHub) state. It resolves the last stored
// payload matching target, optionally resets the Keyed binding (FreshSession),
// and drives one fresh turn. It returns the resolved DispatchResult(s), or an
// error: ErrNoRetryTarget (no selector), ErrHandlerNotFound, or ErrEventNotFound.
func (b *Bus) Retry(handler string, target RetryTarget) ([]DispatchResult, error) {
	if target.Key == "" && target.EventID == "" && target.Subject == "" {
		return nil, ErrNoRetryTarget
	}
	b.mu.Lock()
	var reg *registered
	for _, r := range b.subs {
		if r.sub.Name == handler {
			reg = r
			break
		}
	}
	b.mu.Unlock()
	if reg == nil {
		return nil, ErrHandlerNotFound
	}

	e, ok := reg.store.lookupEvent(target.Key, target.EventID, target.Subject)
	if !ok {
		return nil, ErrEventNotFound
	}

	if target.FreshSession {
		if p, isKeyed := reg.sub.Policy.(Keyed); isKeyed {
			reg.store.unbind(p.Key(e))
		}
	}

	sid, prompt, reused, err := b.resolve(reg, e)
	if err == nil && prompt != "" {
		err = b.driver.SendUserMessage(sid, prompt)
		// Mirror Dispatch's recovery: a replayed turn onto a reused session whose
		// agent registration was orphaned (scheduler restart) 404s → refresh once.
		if err != nil && reused {
			if newSID, rerr := b.refresh(reg, e); rerr == nil && newSID != "" {
				sid = newSID
				err = b.driver.SendUserMessage(sid, prompt)
			}
		}
	}
	return []DispatchResult{{Subscription: reg.sub.Name, EventID: e.ID, SessionID: sid, Err: err}}, nil
}

// policyKey returns the Keyed policy key for an event, or "" for a non-keyed
// policy — the value recorded alongside a stored event so a retry can select by
// handler key.
func policyKey(sub Subscription, e Event) string {
	if p, ok := sub.Policy.(Keyed); ok {
		return p.Key(e)
	}
	return ""
}

// resolve produces the (sessionID, prompt) for one matched subscription per its
// policy. reused is true when an existing session was reused rather than freshly
// created — the caller uses it to gate 404 recovery.
func (b *Bus) resolve(r *registered, e Event) (sid, prompt string, reused bool, err error) {
	switch p := r.sub.Policy.(type) {
	case Stateless:
		sid, err := b.driver.CreateSession(p.Agent, p.EnvID)
		if err != nil {
			return "", "", false, err
		}
		r.store.addCreated(sid)
		return sid, p.Prompt(e), false, nil

	case Keyed:
		key := p.Key(e)
		if sid, ok := r.store.binding(key); ok && b.alive(sid) {
			return sid, p.Prompt(e), true, nil
		}
		sid, err := b.driver.CreateSession(p.Agent, p.EnvID)
		if err != nil {
			return "", "", false, err
		}
		r.store.bind(key, sid)
		r.store.addCreated(sid)
		return sid, p.Prompt(e), false, nil

	case Routed:
		return b.resolveRouted(r, p, e)

	default:
		return "", "", false, fmt.Errorf("eventbus: unknown policy %T", r.sub.Policy)
	}
}

// refresh creates a fresh session for a keyed/routed subscription and re-points
// its binding (keyed only), used to recover from a reused session that rejected a
// turn. Returns "" for a Stateless policy (which never reuses).
func (b *Bus) refresh(r *registered, e Event) (string, error) {
	var agent AgentRef
	var envID string
	switch p := r.sub.Policy.(type) {
	case Keyed:
		agent, envID = p.Agent, p.EnvID
	case Routed:
		agent, envID = p.Agent, p.EnvID
	default:
		return "", nil
	}
	sid, err := b.driver.CreateSession(agent, envID)
	if err != nil {
		return "", err
	}
	if p, ok := r.sub.Policy.(Keyed); ok {
		r.store.bind(p.Key(e), sid) // overwrite the stale binding
	}
	r.store.addCreated(sid)
	return sid, nil
}

// alive reports whether a session exists and is still reusable (not archived and
// not in a terminal status). A terminated/pre-restart session is NOT alive, so a
// keyed policy will create a fresh one instead of streaming to a dead session.
func (b *Bus) alive(sessionID string) bool {
	s, err := b.driver.SessionSummary(sessionID)
	return err == nil && s.Reusable()
}

func (b *Bus) resolveRouted(r *registered, p Routed, e Event) (string, string, bool, error) {
	max := p.Router.MaxCandidates
	if max <= 0 {
		max = defaultMaxCandidates
	}
	var cands []SessionSummary
	for _, sid := range r.store.recentCreated(max) {
		s, err := b.driver.SessionSummary(sid)
		if err != nil || !s.Reusable() {
			continue
		}
		cands = append(cands, s)
	}

	dec := b.routerDecide(p, e, cands)
	prompt := dec.Prompt
	if prompt == "" && p.Prompt != nil {
		prompt = p.Prompt(e)
	}

	// Reuse only a candidate that is valid + reusable; otherwise create new.
	if dec.ReuseSessionID != "" && candidateContains(cands, dec.ReuseSessionID) {
		return dec.ReuseSessionID, prompt, true, nil
	}
	sid, err := b.driver.CreateSession(p.Agent, p.EnvID)
	if err != nil {
		return "", "", false, err
	}
	r.store.addCreated(sid)
	return sid, prompt, false, nil
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
