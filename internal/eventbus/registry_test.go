package eventbus

import (
	"context"
	"encoding/json"
	"testing"
)

func prEvent(iid string) Event {
	b, _ := json.Marshal(map[string]any{"iid": iid})
	return Event{ID: "pr-" + iid, Type: "pr", Subject: iid, Payload: b}
}

func keyedHandlerSpec(name string) HandlerSpec {
	return HandlerSpec{
		Name:  name,
		Match: MatchSpec{Type: "pr"},
		Policy: PolicySpec{
			Kind:           "keyed",
			AgentID:        "agent_x",
			KeyTemplate:    "{{.subject}}",
			PromptTemplate: "review {{.subject}}",
		},
	}
}

func TestRegistry_AddHandlerDispatches(t *testing.T) {
	f := newFake()
	bus := New(f, t.TempDir(), 8)
	reg, err := NewRegistry(context.Background(), bus, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AddHandler(keyedHandlerSpec("h1")); err != nil {
		t.Fatal(err)
	}
	bus.Dispatch(prEvent("3177"))
	if f.sentCount() != 1 {
		t.Fatalf("want 1 sent, got %d", f.sentCount())
	}
	if got := f.lastSent().prompt; got != "review 3177" {
		t.Errorf("prompt = %q", got)
	}
	if len(reg.ListHandlers()) != 1 {
		t.Errorf("want 1 handler listed")
	}
}

func TestRegistry_DuplicateHandlerRejected(t *testing.T) {
	bus := New(newFake(), t.TempDir(), 8)
	reg, _ := NewRegistry(context.Background(), bus, t.TempDir())
	if err := reg.AddHandler(keyedHandlerSpec("h1")); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddHandler(keyedHandlerSpec("h1")); err == nil {
		t.Error("want error on duplicate handler name")
	}
}

func TestRegistry_RemoveHandlerStopsDispatch(t *testing.T) {
	f := newFake()
	bus := New(f, t.TempDir(), 8)
	reg, _ := NewRegistry(context.Background(), bus, t.TempDir())
	_ = reg.AddHandler(keyedHandlerSpec("h1"))
	ok, err := reg.RemoveHandler("h1")
	if err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	bus.Dispatch(prEvent("3177"))
	if f.sentCount() != 0 {
		t.Errorf("removed handler still dispatched: %d", f.sentCount())
	}
	if missing, _ := reg.RemoveHandler("nope"); missing {
		t.Error("removing unknown handler should report false")
	}
}

// Persisted specs must survive a restart: a fresh Registry over the same dir
// rebuilds the handler and dispatch works again.
func TestRegistry_RebuildFromDisk(t *testing.T) {
	dir := t.TempDir()
	{
		bus := New(newFake(), t.TempDir(), 8)
		reg, _ := NewRegistry(context.Background(), bus, dir)
		if err := reg.AddHandler(keyedHandlerSpec("h1")); err != nil {
			t.Fatal(err)
		}
	}
	// New process: new bus + driver, same registry dir.
	f2 := newFake()
	bus2 := New(f2, t.TempDir(), 8)
	reg2, err := NewRegistry(context.Background(), bus2, dir)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if len(reg2.ListHandlers()) != 1 {
		t.Fatalf("want 1 rebuilt handler, got %d", len(reg2.ListHandlers()))
	}
	bus2.Dispatch(prEvent("42"))
	if f2.sentCount() != 1 {
		t.Errorf("rebuilt handler did not dispatch: %d", f2.sentCount())
	}
}

func TestBuildFetch(t *testing.T) {
	if _, err := buildFetch(SourceSpec{Type: "codehub-pr", Project: "ns/proj"}); err != nil {
		t.Errorf("valid codehub-pr: %v", err)
	}
	if _, err := buildFetch(SourceSpec{Type: "codehub-pr"}); err == nil {
		t.Error("codehub-pr without project should error")
	}
	if _, err := buildFetch(SourceSpec{Type: "what"}); err == nil {
		t.Error("unknown type should error")
	}
}

func TestRegistry_SourceLifecycle(t *testing.T) {
	bus := New(newFake(), t.TempDir(), 8)
	reg, _ := NewRegistry(context.Background(), bus, t.TempDir())
	// /usr/bin/true exits 0 with empty output → fetch errors are logged and
	// ignored; the source just records/persists without dispatching.
	spec := SourceSpec{Name: "s1", Type: "codehub-pr", Project: "ns/proj", Interval: "1h", Bin: "/usr/bin/true"}
	if err := reg.AddSource(spec); err != nil {
		t.Fatal(err)
	}
	if len(reg.ListSources()) != 1 {
		t.Fatalf("want 1 source")
	}
	if err := reg.AddSource(spec); err == nil {
		t.Error("duplicate source name should error")
	}
	ok, err := reg.RemoveSource("s1")
	if err != nil || !ok {
		t.Fatalf("remove source: ok=%v err=%v", ok, err)
	}
	if len(reg.ListSources()) != 0 {
		t.Error("source not removed")
	}
}
