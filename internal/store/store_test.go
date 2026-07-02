package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wu8685/cma-service/internal/cma"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestAgentVersioning(t *testing.T) {
	s := newTestStore(t)
	id := "agent_v"
	if err := s.PutAgentVersion(&cma.Agent{ID: id, Version: 1, Name: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgentVersion(&cma.Agent{ID: id, Version: 2, Name: "v2"}); err != nil {
		t.Fatal(err)
	}

	// version 0 resolves to latest
	a, ok := s.Agent(id, 0)
	if !ok || a.Version != 2 || a.Name != "v2" {
		t.Fatalf("latest = %+v ok=%v", a, ok)
	}
	// explicit older version is retrievable
	a1, ok := s.Agent(id, 1)
	if !ok || a1.Name != "v1" {
		t.Fatalf("v1 = %+v ok=%v", a1, ok)
	}
	// unknown version
	if _, ok := s.Agent(id, 99); ok {
		t.Error("expected miss for unknown version")
	}
	// ListAgents returns only the latest of each
	list := s.ListAgents()
	if len(list) != 1 || list[0].Version != 2 {
		t.Fatalf("ListAgents = %+v", list)
	}
}

func mkSession(id string) *SessionRecord {
	return &SessionRecord{
		Session:   &cma.Session{Type: "session", ID: id, Status: cma.StatusIdle},
		AhsirName: "cma-x-v1",
		ContextID: "ctx_1",
	}
}

func TestEventAppendAndSnapshot(t *testing.T) {
	s := newTestStore(t)
	rec := mkSession("sesn_1")
	if err := s.PutSession(rec); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.AppendEvent(rec, cma.Event{ID: "ev_" + string(rune('a'+i)), Type: "agent.message"}); err != nil {
			t.Fatal(err)
		}
	}
	snap := rec.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d", len(snap))
	}
	// Snapshot is a copy: mutating it must not affect the log.
	snap[0].Type = "mutated"
	if rec.Snapshot()[0].Type != "agent.message" {
		t.Error("Snapshot did not return a copy")
	}
}

func TestSubscribeReceivesLiveEvents(t *testing.T) {
	s := newTestStore(t)
	rec := mkSession("sesn_2")
	_ = s.PutSession(rec)

	_, ch, cancel := rec.Subscribe()
	defer cancel()

	// AppendEvent notifies subscribers before it persists, so capture the
	// goroutine's completion and wait for it below — otherwise the async
	// persist() (WriteFile tmp + rename) can race t.TempDir's RemoveAll and
	// recreate a file mid-cleanup ("directory not empty").
	done := make(chan error, 1)
	go func() { done <- s.AppendEvent(rec, cma.Event{ID: "ev_x", Type: "agent.message"}) }()

	select {
	case ev := <-ch:
		if ev.ID != "ev_x" {
			t.Errorf("got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive live event")
	}

	if err := <-done; err != nil {
		t.Errorf("AppendEvent: %v", err)
	}
}

// TestEnqueueTurnSerial is the regression for turn serialization: jobs for one
// session must never run concurrently, and must run in FIFO order.
func TestEnqueueTurnSerial(t *testing.T) {
	rec := mkSession("sesn_3")

	const n = 20
	var concurrent, maxConcurrent int32
	var order []int
	var orderMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		i := i
		rec.EnqueueTurn(func() {
			cur := atomic.AddInt32(&concurrent, 1)
			for {
				m := atomic.LoadInt32(&maxConcurrent)
				if cur <= m || atomic.CompareAndSwapInt32(&maxConcurrent, m, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond) // widen the window for any overlap
			orderMu.Lock()
			order = append(order, i)
			orderMu.Unlock()
			atomic.AddInt32(&concurrent, -1)
			wg.Done()
		})
	}
	wg.Wait()

	if maxConcurrent != 1 {
		t.Fatalf("max concurrent turns = %d, want 1 (turns interleaved)", maxConcurrent)
	}
	for i := 0; i < n; i++ {
		if order[i] != i {
			t.Fatalf("turn order not FIFO: %v", order)
		}
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.PutAgentVersion(&cma.Agent{ID: "agent_p", Version: 1, Name: "p"})
	rec := mkSession("sesn_p")
	_ = s.PutSession(rec)
	_ = s.AppendEvent(rec, cma.Event{ID: "ev_1", Type: "agent.message"})

	// Reload from disk into a fresh store.
	s2, err := New(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if a, ok := s2.Agent("agent_p", 0); !ok || a.Name != "p" {
		t.Errorf("agent not persisted: %+v ok=%v", a, ok)
	}
	rec2, ok := s2.Session("sesn_p")
	if !ok {
		t.Fatal("session not persisted")
	}
	if got := rec2.Snapshot(); len(got) != 1 || got[0].ID != "ev_1" {
		t.Errorf("events not persisted: %+v", got)
	}
	// A reloaded record must be usable as a live subscription target.
	if _, _, cancel := rec2.Subscribe(); cancel != nil {
		cancel()
	} else {
		t.Error("reloaded record not subscribable")
	}
}
