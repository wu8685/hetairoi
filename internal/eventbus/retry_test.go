package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// keyedSub builds a keyed subscription keyed on Subject, prompting with the
// event's decoded payload note so a retry replay is observable.
func keyedSub(name string) Subscription {
	return Subscription{
		Name:  name,
		Match: func(Event) bool { return true },
		Policy: Keyed{
			Agent:  AgentRef{ID: "a"},
			Key:    func(e Event) string { return e.Subject },
			Prompt: func(e Event) string { return "work:" + e.ID },
		},
	}
}

func issueEvent(id, subject, note string) Event {
	b, _ := json.Marshal(map[string]any{"note": note})
	return Event{ID: id, Type: "issue", Subject: subject, Payload: b}
}

// §Acceptance: a handler that already processed an event re-runs it (fresh
// turn) with zero upstream change, replaying the stored payload.
func TestRetry_ReplaysLastPayload(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(keyedSub("h"))

	b.Dispatch(issueEvent("gh-issue-6-abc", "6", "hello"))
	if f.sentCount() != 1 {
		t.Fatalf("initial dispatch sent %d, want 1", f.sentCount())
	}
	first := f.lastSent()

	res, err := b.Retry("h", RetryTarget{Key: "6"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if f.sentCount() != 2 {
		t.Fatalf("after retry sent %d, want 2 (replayed)", f.sentCount())
	}
	if res[0].SessionID != first.sessionID {
		t.Fatalf("alive keyed session should be reused: retry=%s first=%s", res[0].SessionID, first.sessionID)
	}
	if got := f.lastSent().prompt; got != "work:gh-issue-6-abc" {
		t.Fatalf("replayed prompt = %q, want the stored payload's", got)
	}
}

// Retry bypasses dedup: the same Event.ID re-runs even though a normal second
// dispatch would be skipped.
func TestRetry_BypassesDedup(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(keyedSub("h"))

	b.Dispatch(issueEvent("gh-issue-6-abc", "6", "x"))
	// A normal re-dispatch of the same id is deduped.
	if r := b.Dispatch(issueEvent("gh-issue-6-abc", "6", "x")); !r[0].Skipped {
		t.Fatal("precondition: second dispatch should be deduped")
	}
	if f.sentCount() != 1 {
		t.Fatalf("after dedup sent %d, want 1", f.sentCount())
	}
	// Retry by event_id ignores dedup and re-runs.
	if _, err := b.Retry("h", RetryTarget{EventID: "gh-issue-6-abc"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if f.sentCount() != 2 {
		t.Fatalf("retry should bypass dedup; sent %d, want 2", f.sentCount())
	}
}

// fresh_session resets the keyed binding so a NEW session is created even when
// the bound one is still alive; default reuses the alive one.
func TestRetry_FreshSessionResetsBinding(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(keyedSub("h"))

	first := b.Dispatch(issueEvent("gh-issue-6-a", "6", "x"))
	bound := first[0].SessionID

	// Default retry: alive binding reused.
	reuse, err := b.Retry("h", RetryTarget{Key: "6"})
	if err != nil {
		t.Fatal(err)
	}
	if reuse[0].SessionID != bound {
		t.Fatalf("default retry should reuse alive session; got %s want %s", reuse[0].SessionID, bound)
	}

	// fresh_session: new session, and the binding is rebound to it.
	fresh, err := b.Retry("h", RetryTarget{Key: "6", FreshSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh[0].SessionID == bound {
		t.Fatalf("fresh_session should create a new session; got the bound one %s", bound)
	}
	if sid, ok := reg(b, "h").store.binding("6"); !ok || sid != fresh[0].SessionID {
		t.Fatalf("binding not reset to fresh session: got %s (ok=%v) want %s", sid, ok, fresh[0].SessionID)
	}
}

// Acceptance: a terminated (archived) bound session is replaced on a plain
// retry — no fresh_session needed, no upstream change.
func TestRetry_TerminatedBoundSessionReplaced(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(keyedSub("h"))

	first := b.Dispatch(issueEvent("gh-issue-6-a", "6", "x"))
	bound := first[0].SessionID
	f.summaries[bound] = SessionSummary{SessionID: bound, Archived: true}

	res, err := b.Retry("h", RetryTarget{Key: "6"})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].SessionID == bound {
		t.Fatal("a terminated bound session must be replaced on retry")
	}
	if f.sentCount() != 2 {
		t.Fatalf("retry should drive a fresh turn; sent %d, want 2", f.sentCount())
	}
}

// Retry can select by subject (most recent stored event with that subject).
func TestRetry_BySubject(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(keyedSub("h"))
	b.Dispatch(issueEvent("gh-issue-7-a", "7", "x"))

	if _, err := b.Retry("h", RetryTarget{Subject: "7"}); err != nil {
		t.Fatalf("retry by subject: %v", err)
	}
	if f.sentCount() != 2 {
		t.Fatalf("sent %d, want 2", f.sentCount())
	}
}

func TestRetry_UnknownHandler(t *testing.T) {
	b := New(newFake(), t.TempDir(), 8)
	_ = b.Register(keyedSub("h"))
	if _, err := b.Retry("nope", RetryTarget{Key: "6"}); !errors.Is(err, ErrHandlerNotFound) {
		t.Fatalf("err = %v, want ErrHandlerNotFound", err)
	}
}

func TestRetry_UnknownKey(t *testing.T) {
	b := New(newFake(), t.TempDir(), 8)
	_ = b.Register(keyedSub("h"))
	b.Dispatch(issueEvent("gh-issue-6-a", "6", "x"))
	if _, err := b.Retry("h", RetryTarget{Key: "999"}); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("err = %v, want ErrEventNotFound", err)
	}
	if _, err := b.Retry("h", RetryTarget{EventID: "gh-issue-none"}); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("err = %v, want ErrEventNotFound", err)
	}
}

func TestRetry_NoTarget(t *testing.T) {
	b := New(newFake(), t.TempDir(), 8)
	_ = b.Register(keyedSub("h"))
	if _, err := b.Retry("h", RetryTarget{}); !errors.Is(err, ErrNoRetryTarget) {
		t.Fatalf("err = %v, want ErrNoRetryTarget", err)
	}
}

// The last-payload replay window survives a restart: a fresh Bus over the same
// dir rebuilds the stored event and a retry replays it.
func TestRetry_PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	f1 := newFake()
	b1 := New(f1, dir, 8)
	_ = b1.Register(keyedSub("h"))
	b1.Dispatch(issueEvent("gh-issue-6-a", "6", "hello"))

	// New process: fresh bus + driver, same state dir.
	f2 := newFake()
	b2 := New(f2, dir, 8)
	_ = b2.Register(keyedSub("h"))
	res, err := b2.Retry("h", RetryTarget{Key: "6"})
	if err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if f2.sentCount() != 1 || res[0].EventID != "gh-issue-6-a" {
		t.Fatalf("stored payload did not survive restart: sent=%d res=%+v", f2.sentCount(), res[0])
	}
}

// A stateless handler has no key, so retry-by-key finds nothing, but
// retry-by-event_id / subject still replays.
func TestRetry_StatelessByEventID(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "h", Match: func(Event) bool { return true },
		Policy: Stateless{Agent: AgentRef{ID: "a"}, Prompt: func(e Event) string { return "s:" + e.ID }}})
	b.Dispatch(issueEvent("gh-issue-8-a", "8", "x"))

	if _, err := b.Retry("h", RetryTarget{Key: "8"}); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("stateless retry-by-key err = %v, want ErrEventNotFound", err)
	}
	if _, err := b.Retry("h", RetryTarget{EventID: "gh-issue-8-a"}); err != nil {
		t.Fatalf("stateless retry-by-event_id: %v", err)
	}
	if f.sentCount() != 2 {
		t.Fatalf("sent %d, want 2 (stateless replay creates a new session)", f.sentCount())
	}
}

// Retry is also reachable through the Registry (the control-plane wrapper).
func TestRegistry_Retry(t *testing.T) {
	f := newFake()
	bus := New(f, t.TempDir(), 8)
	r, err := NewRegistry(context.Background(), bus, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = r.AddHandler(keyedHandlerSpec("h1"))
	bus.Dispatch(prEvent("3177"))
	if _, err := r.Retry("h1", RetryTarget{Subject: "3177"}); err != nil {
		t.Fatalf("registry retry: %v", err)
	}
	if f.sentCount() != 2 {
		t.Fatalf("sent %d, want 2", f.sentCount())
	}
	if _, err := r.Retry("missing", RetryTarget{Subject: "3177"}); !errors.Is(err, ErrHandlerNotFound) {
		t.Fatalf("err = %v, want ErrHandlerNotFound", err)
	}
}

// recordEvent keeps one entry per Event.ID (newest position) and bounds the
// window to maxSeen — exercised white-box since dedup normally hides both paths.
func TestSubStore_RecordEventReplaceAndOverflow(t *testing.T) {
	s, err := newSubStore(t.TempDir(), "h", DedupConfig{MaxEntries: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Same ID recorded twice → a single entry, latest payload wins.
	s.recordEvent("k", Event{ID: "a", Subject: "old"})
	s.recordEvent("k", Event{ID: "a", Subject: "new"})
	if len(s.state.Events) != 1 {
		t.Fatalf("same-id record should replace, len=%d", len(s.state.Events))
	}
	if e, _ := s.lookupEvent("", "a", ""); e.Subject != "new" {
		t.Fatalf("replaced entry should carry the newest payload, got %q", e.Subject)
	}
	// Overflow past maxSeen (2) evicts the oldest.
	s.recordEvent("k", Event{ID: "b"})
	s.recordEvent("k", Event{ID: "c"})
	if len(s.state.Events) != 2 {
		t.Fatalf("window should cap at 2, len=%d", len(s.state.Events))
	}
	if _, ok := s.lookupEvent("", "a", ""); ok {
		t.Fatal("oldest event 'a' should have been evicted")
	}
}

// unbind on an absent key is a no-op (no panic, no persist error).
func TestSubStore_UnbindAbsentKey(t *testing.T) {
	s, err := newSubStore(t.TempDir(), "h", DedupConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s.unbind("never-bound") // must not panic
	s.bind("k", "sesn_1")
	s.unbind("k")
	if _, ok := s.binding("k"); ok {
		t.Fatal("binding should be gone after unbind")
	}
}

// reg is a test helper to reach a registered subscription's store.
func reg(b *Bus, name string) *registered {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.subs {
		if r.sub.Name == name {
			return r
		}
	}
	return nil
}
