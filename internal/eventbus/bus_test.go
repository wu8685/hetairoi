package eventbus

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeDriver is a scriptable SessionDriver for unit tests.
type fakeDriver struct {
	mu               sync.Mutex
	nextID           int
	created          []AgentRef
	sent             []sent
	summaries        map[string]SessionSummary // overrides (e.g. archived)
	sendErr          map[string]error          // per-session SendUserMessage failures (e.g. 404)
	routerReply      string
	routerErr        error
	lastRouterPrompt string
}

type sent struct {
	sessionID string
	prompt    string
}

func newFake() *fakeDriver {
	return &fakeDriver{summaries: map[string]SessionSummary{}, sendErr: map[string]error{}}
}

func (f *fakeDriver) CreateSession(agent AgentRef, envID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("sesn_%d", f.nextID)
	f.created = append(f.created, agent)
	return id, nil
}

func (f *fakeDriver) SendUserMessage(sessionID, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sent{sessionID, prompt})
	return f.sendErr[sessionID]
}

func (f *fakeDriver) RunForReply(agent AgentRef, envID, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRouterPrompt = prompt
	return f.routerReply, f.routerErr
}

func (f *fakeDriver) SessionSummary(sessionID string) (SessionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.summaries[sessionID]; ok {
		return s, nil
	}
	return SessionSummary{SessionID: sessionID, Seed: "seed-" + sessionID, Last: "last-" + sessionID}, nil
}

func (f *fakeDriver) sentCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }
func (f *fakeDriver) lastSent() sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

func stateless(prompt string) Stateless {
	return Stateless{Agent: AgentRef{ID: "agent_x"}, Prompt: func(Event) string { return prompt }}
}

// ---- §12.1 match ----

func TestDispatch_MatchRouting(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "alerts", Match: func(e Event) bool { return e.Type == "alert" }, Policy: stateless("p")})
	_ = b.Register(Subscription{Name: "all", Match: func(e Event) bool { return true }, Policy: stateless("p")})

	res := b.Dispatch(Event{ID: "1", Type: "alert"})
	if len(res) != 2 { // both "alerts" and "all" match
		t.Fatalf("alert matched %d subs, want 2", len(res))
	}
	res = b.Dispatch(Event{ID: "2", Type: "chat"})
	if len(res) != 1 || res[0].Subscription != "all" {
		t.Fatalf("chat matched %+v, want only 'all'", res)
	}
}

// ---- §12.2 dedup ----

func TestDispatch_Dedup(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true }, Policy: stateless("p")})

	b.Dispatch(Event{ID: "dup"})
	res := b.Dispatch(Event{ID: "dup"})
	if len(res) != 1 || !res[0].Skipped {
		t.Fatalf("second dispatch = %+v, want skipped", res)
	}
	if f.sentCount() != 1 {
		t.Fatalf("sent %d, want 1 (dedup)", f.sentCount())
	}
}

// ---- §12.3 dedup persistence + rotate ----

func TestDedup_PersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	b1 := New(f, dir, 8)
	_ = b1.Register(Subscription{Name: "s", Match: func(Event) bool { return true }, Policy: stateless("p")})
	b1.Dispatch(Event{ID: "x"})

	// Fresh bus, same dir → dedup state is loaded.
	b2 := New(f, dir, 8)
	_ = b2.Register(Subscription{Name: "s", Match: func(Event) bool { return true }, Policy: stateless("p")})
	res := b2.Dispatch(Event{ID: "x"})
	if !res[0].Skipped {
		t.Fatal("dedup did not survive restart")
	}
}

func TestDedup_RotateReprocesses(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{
		Name: "s", Match: func(Event) bool { return true }, Policy: stateless("p"),
		Dedup: DedupConfig{MaxEntries: 2},
	})
	b.Dispatch(Event{ID: "a"})
	b.Dispatch(Event{ID: "b"})
	b.Dispatch(Event{ID: "c"}) // window now {b,c}; "a" rotated out
	res := b.Dispatch(Event{ID: "a"})
	if res[0].Skipped {
		t.Fatal("id 'a' should reprocess after rotating out of the window")
	}
}

// ---- §12.4 hop guard ----

func TestDispatch_HopGuard(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 3)
	_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true }, Policy: stateless("p")})
	if res := b.Dispatch(Event{ID: "1", Hop: 4}); res != nil {
		t.Fatalf("hop>max should be rejected, got %+v", res)
	}
	if f.sentCount() != 0 {
		t.Fatal("nothing should be sent for a rejected event")
	}
}

// ---- §12.5 Stateless ----

func TestStateless_NewSessionPerEvent(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true },
		Policy: Stateless{Agent: AgentRef{ID: "a"}, Prompt: func(e Event) string { return "do:" + e.ID }}})
	r1 := b.Dispatch(Event{ID: "1"})
	r2 := b.Dispatch(Event{ID: "2"})
	if r1[0].SessionID == r2[0].SessionID {
		t.Fatal("stateless must create a new session per event")
	}
	if f.lastSent().prompt != "do:2" {
		t.Fatalf("prompt = %q", f.lastSent().prompt)
	}
}

// ---- §12.6 Keyed ----

func TestKeyed_ReuseByKey(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true },
		Policy: Keyed{Agent: AgentRef{ID: "a"}, Key: func(e Event) string { return e.Subject }, Prompt: func(e Event) string { return e.ID }}})

	a := b.Dispatch(Event{ID: "1", Subject: "thread-1"})
	a2 := b.Dispatch(Event{ID: "2", Subject: "thread-1"})
	if a[0].SessionID != a2[0].SessionID {
		t.Fatal("same key must reuse the session")
	}
	other := b.Dispatch(Event{ID: "3", Subject: "thread-2"})
	if other[0].SessionID == a[0].SessionID {
		t.Fatal("different key must use a new session")
	}
}

func TestKeyed_ArchivedBoundReplaced(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true },
		Policy: Keyed{Agent: AgentRef{ID: "a"}, Key: func(e Event) string { return e.Subject }, Prompt: func(Event) string { return "p" }}})

	first := b.Dispatch(Event{ID: "1", Subject: "k"})
	// archive the bound session
	f.summaries[first[0].SessionID] = SessionSummary{SessionID: first[0].SessionID, Archived: true}
	second := b.Dispatch(Event{ID: "2", Subject: "k"})
	if second[0].SessionID == first[0].SessionID {
		t.Fatal("an archived bound session must be replaced by a new one")
	}
}

// Regression for #4: a keyed binding pointing at a terminated (or errored)
// session — e.g. one killed by a scheduler restart — must NOT be reused. The next
// dispatch has to create a fresh session and re-point the binding, otherwise
// every retrigger streams to the dead session and 404s.
func TestKeyed_TerminatedBoundReplaced(t *testing.T) {
	for _, status := range []string{StatusTerminated, StatusErrored, StatusError} {
		t.Run(status, func(t *testing.T) {
			f := newFake()
			b := New(f, t.TempDir(), 8)
			_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true },
				Policy: Keyed{Agent: AgentRef{ID: "a"}, Key: func(e Event) string { return e.Subject }, Prompt: func(Event) string { return "p" }}})

			first := b.Dispatch(Event{ID: "1", Subject: "k"})
			// The bound session is now terminated (exists, not archived — the old
			// "alive" definition would have wrongly reused it).
			f.summaries[first[0].SessionID] = SessionSummary{SessionID: first[0].SessionID, Status: status}

			second := b.Dispatch(Event{ID: "2", Subject: "k"})
			if second[0].SessionID == first[0].SessionID {
				t.Fatalf("a %s bound session must be replaced by a fresh one", status)
			}
			if second[0].Err != nil {
				t.Fatalf("fresh session dispatch errored: %v", second[0].Err)
			}
			// The binding must now follow the fresh session, so a third event reuses it
			// (not the dead one, and not yet-another new session).
			third := b.Dispatch(Event{ID: "3", Subject: "k"})
			if third[0].SessionID != second[0].SessionID {
				t.Fatalf("binding not re-pointed: third=%s want %s", third[0].SessionID, second[0].SessionID)
			}
		})
	}
}

// Regression for #4: even when the bound session still *looks* reusable (status
// unknown), a reused turn that 404s — because the scheduler restart orphaned the
// agent's inline registration — must self-recover: drop the binding, create a
// fresh (re-registered) session, and retry the turn once.
func TestKeyed_ReuseRecoversOnSendFailure(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true },
		Policy: Keyed{Agent: AgentRef{ID: "a"}, Key: func(e Event) string { return e.Subject }, Prompt: func(e Event) string { return "p:" + e.ID }}})

	first := b.Dispatch(Event{ID: "1", Subject: "k"})
	dead := first[0].SessionID
	// The bound session still reports reusable, but its turn now 404s.
	f.sendErr[dead] = fmt.Errorf("stream %q: agent not found (404)", dead)

	second := b.Dispatch(Event{ID: "2", Subject: "k"})
	if second[0].SessionID == dead {
		t.Fatal("a reused session whose turn 404s must be replaced, not kept")
	}
	if second[0].Err != nil {
		t.Fatalf("recovery should have retried on a fresh session, got err: %v", second[0].Err)
	}
	// The retry re-sent the SAME prompt to the fresh session.
	if got := f.lastSent(); got.sessionID != second[0].SessionID || got.prompt != "p:2" {
		t.Fatalf("retry sent %+v, want prompt p:2 on %s", got, second[0].SessionID)
	}
	// Binding now points at the fresh session — the next event reuses it cleanly.
	third := b.Dispatch(Event{ID: "3", Subject: "k"})
	if third[0].SessionID != second[0].SessionID {
		t.Fatalf("binding not re-pointed after recovery: third=%s want %s", third[0].SessionID, second[0].SessionID)
	}
}

// A stateless turn that fails must NOT trigger reuse-recovery (there is no session
// to replace) — the error surfaces as-is.
func TestStateless_SendFailureNotRecovered(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true }, Policy: stateless("p")})

	// Fail the first session that will be created.
	f.sendErr["sesn_1"] = fmt.Errorf("boom")
	res := b.Dispatch(Event{ID: "1"})
	if res[0].Err == nil {
		t.Fatal("stateless send failure must surface, not be silently recovered")
	}
	if f.sentCount() != 1 {
		t.Fatalf("stateless must not retry on a fresh session; sent %d", f.sentCount())
	}
}

// A terminated candidate must be excluded from the Routed router's candidate set,
// so the router is never offered a dead session to reuse.
func TestRouted_TerminatedCandidateExcluded(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(routedSub("s", 20))

	f.routerReply = `{"reuse_session_id":"","prompt":"first"}`
	first := b.Dispatch(Event{ID: "1"})
	sid := first[0].SessionID
	// Terminate that session; it must no longer be a reuse candidate.
	f.summaries[sid] = SessionSummary{SessionID: sid, Status: StatusTerminated}

	f.routerReply = fmt.Sprintf(`{"reuse_session_id":%q,"prompt":"second"}`, sid)
	second := b.Dispatch(Event{ID: "2"})
	if second[0].SessionID == sid {
		t.Fatal("a terminated candidate must not be reused by the router")
	}
	if strings.Contains(f.lastRouterPrompt, sid) {
		t.Fatalf("terminated session %s should be excluded from the router prompt: %s", sid, f.lastRouterPrompt)
	}
}

// A routed session the router chose to reuse whose turn 404s must also self-
// recover onto a fresh session (exercises refresh's Routed branch).
func TestRouted_ReuseRecoversOnSendFailure(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(routedSub("s", 20))

	f.routerReply = `{"reuse_session_id":"","prompt":"first"}`
	first := b.Dispatch(Event{ID: "1"})
	sid := first[0].SessionID

	// Router picks the existing session, but its turn 404s.
	f.routerReply = fmt.Sprintf(`{"reuse_session_id":%q,"prompt":"second"}`, sid)
	f.sendErr[sid] = fmt.Errorf("stream %q: agent not found (404)", sid)

	second := b.Dispatch(Event{ID: "2"})
	if second[0].SessionID == sid {
		t.Fatal("a reused routed session whose turn 404s must be replaced")
	}
	if second[0].Err != nil {
		t.Fatalf("routed recovery should have retried on a fresh session, got: %v", second[0].Err)
	}
	if got := f.lastSent(); got.sessionID != second[0].SessionID || got.prompt != "second" {
		t.Fatalf("retry sent %+v, want prompt 'second' on the fresh session", got)
	}
}

func TestSessionSummary_Reusable(t *testing.T) {
	cases := []struct {
		name string
		s    SessionSummary
		want bool
	}{
		{"empty-status-runnable", SessionSummary{}, true},
		{"running", SessionSummary{Status: "running"}, true},
		{"idle", SessionSummary{Status: "idle"}, true},
		{"rescheduling", SessionSummary{Status: "rescheduling"}, true},
		{"archived", SessionSummary{Archived: true}, false},
		{"archived-beats-running", SessionSummary{Archived: true, Status: "running"}, false},
		{"terminated", SessionSummary{Status: StatusTerminated}, false},
		{"errored", SessionSummary{Status: StatusErrored}, false},
		{"error", SessionSummary{Status: StatusError}, false},
	}
	for _, c := range cases {
		if got := c.s.Reusable(); got != c.want {
			t.Errorf("%s: Reusable()=%v, want %v", c.name, got, c.want)
		}
	}
}

// ---- §12.7–9 Routed ----

func routedSub(name string, max int) Subscription {
	return Subscription{Name: name, Match: func(Event) bool { return true },
		Policy: Routed{
			Agent: AgentRef{ID: "handler"}, EnvID: "e",
			Router: RouterSpec{Agent: AgentRef{ID: "router"}, MaxCandidates: max},
			Prompt: func(e Event) string { return "fallback:" + e.ID },
		}}
}

func TestRouted_Reuse(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(routedSub("s", 20))

	f.routerReply = `{"reuse_session_id":"","prompt":"first"}`
	first := b.Dispatch(Event{ID: "1"}) // → new session (candidate for next)
	sid := first[0].SessionID

	f.routerReply = fmt.Sprintf(`{"reuse_session_id":%q,"prompt":"second"}`, sid)
	second := b.Dispatch(Event{ID: "2"})
	if second[0].SessionID != sid {
		t.Fatalf("routed reuse = %s, want %s", second[0].SessionID, sid)
	}
	if f.lastSent().prompt != "second" {
		t.Fatalf("prompt = %q, want 'second'", f.lastSent().prompt)
	}
}

func TestRouted_NewWhenEmpty(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(routedSub("s", 20))
	f.routerReply = `{"reuse_session_id":"","prompt":"go"}`
	a := b.Dispatch(Event{ID: "1"})
	b2 := b.Dispatch(Event{ID: "2"})
	if a[0].SessionID == b2[0].SessionID {
		t.Fatal("empty reuse → each event gets a new session")
	}
}

func TestRouted_DegradeOnBadReply(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(routedSub("s", 20))

	// unparseable reply → new session, fallback prompt, no crash
	f.routerReply = "sorry I cannot help"
	r := b.Dispatch(Event{ID: "1"})
	if r[0].Err != nil || r[0].SessionID == "" {
		t.Fatalf("bad reply should degrade to a new session, got %+v", r[0])
	}
	if f.lastSent().prompt != "fallback:1" {
		t.Fatalf("prompt = %q, want fallback", f.lastSent().prompt)
	}

	// unknown session id → new session
	f.routerReply = `{"reuse_session_id":"sesn_999","prompt":"x"}`
	r2 := b.Dispatch(Event{ID: "2"})
	if r2[0].SessionID == "sesn_999" {
		t.Fatal("unknown reuse id must not be used")
	}
}

func TestRouted_CandidateCap(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(routedSub("s", 2)) // cap = 2

	f.routerReply = `{"reuse_session_id":"","prompt":"p"}`
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, b.Dispatch(Event{ID: fmt.Sprintf("%d", i)})[0].SessionID)
	}
	// 4th dispatch builds the router prompt from the 2 newest candidates only.
	b.Dispatch(Event{ID: "9"})
	p := f.lastRouterPrompt
	if !strings.Contains(p, ids[2]) || !strings.Contains(p, ids[1]) {
		t.Fatalf("router prompt should contain the 2 newest sessions; prompt=%s", p)
	}
	if strings.Contains(p, ids[0]) {
		t.Fatalf("oldest session %s should be capped out; prompt=%s", ids[0], p)
	}
}

// ---- §12.12 retrievability ----

func TestDispatch_SessionRetrievable(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "s", Match: func(Event) bool { return true }, Policy: stateless("p")})
	res := b.Dispatch(Event{ID: "1"})
	if res[0].SessionID == "" {
		t.Fatal("dispatch result must expose the resolved session id")
	}
}
