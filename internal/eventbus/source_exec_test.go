package eventbus

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sh returns an argv that runs script through /bin/sh -c, the deterministic
// stand-in for a real plugin binary in these tests.
func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

func TestExecSource_FetchParsesEvents(t *testing.T) {
	out := `[
	  {"id":"prcomment-3196-213601693","type":"pr.comment","subject":"3196","payload":{"iid":3196,"note":"hi"}},
	  {"id":"prcomment-3197-9","type":"pr.comment","subject":"3197","payload":{"iid":3197}}
	]`
	s := ExecSource{Name: "pr-comments", Command: sh("echo '" + out + "'"), Dir: t.TempDir()}
	evs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	e := evs[0]
	if e.ID != "prcomment-3196-213601693" {
		t.Errorf("id = %q", e.ID)
	}
	if e.Type != "pr.comment" {
		t.Errorf("type = %q", e.Type)
	}
	if e.Subject != "3196" {
		t.Errorf("subject = %q", e.Subject)
	}
	// hetairoi fills Source and Time.
	if e.Source != "exec:pr-comments" {
		t.Errorf("source = %q, want exec:pr-comments", e.Source)
	}
	if e.Time.IsZero() {
		t.Error("Time not filled")
	}
	// Payload is preserved for handlers to match/template over.
	var p struct {
		IID  int    `json:"iid"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.IID != 3196 || p.Note != "hi" {
		t.Errorf("payload = %+v", p)
	}
}

func TestExecSource_DefaultEventType(t *testing.T) {
	// An event that omits "type" inherits the spec's EventType default.
	s := ExecSource{
		Command:   sh(`echo '[{"id":"x-1","subject":"1"}]'`),
		EventType: "pr.comment",
	}
	evs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "pr.comment" {
		t.Fatalf("want default type applied, got %+v", evs)
	}
	// With no source name, the label degrades to "exec".
	if evs[0].Source != "exec" {
		t.Errorf("source = %q, want exec", evs[0].Source)
	}
}

func TestExecSource_SkipsEmptyID(t *testing.T) {
	// An entry without an id has no stable dedup identity and is dropped.
	s := ExecSource{Command: sh(`echo '[{"id":"","subject":"1"},{"id":"keep","subject":"2"}]'`)}
	evs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ID != "keep" {
		t.Fatalf("empty-id entry not skipped: %+v", evs)
	}
}

func TestExecSource_EmptyArrayNoEvents(t *testing.T) {
	s := ExecSource{Command: sh(`echo '[]'`)}
	evs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("want 0 events, got %d", len(evs))
	}
}

func TestExecSource_NonZeroExitIsError(t *testing.T) {
	s := ExecSource{Command: sh(`echo boom >&2; exit 3`)}
	_, err := s.Fetch(context.Background())
	if err == nil {
		t.Fatal("want error on non-zero exit")
	}
}

func TestExecSource_MalformedJSONIsError(t *testing.T) {
	s := ExecSource{Command: sh(`echo 'not json'`)}
	if _, err := s.Fetch(context.Background()); err == nil {
		t.Fatal("want error on malformed stdout")
	}
}

func TestExecSource_EmptyCommandIsError(t *testing.T) {
	if _, err := (ExecSource{}).Fetch(context.Background()); err == nil {
		t.Fatal("want error on empty command")
	}
}

// The plugin sees HET_PROTOCOL and HET_SCRATCH; the scratch dir is created for it.
func TestExecSource_ExportsProtocolAndScratchEnv(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "s1")
	// Emit the env values back through an event so we can assert them.
	script := `printf '[{"id":"env-%s","type":"probe","subject":"%s"}]' "$HET_PROTOCOL" "$HET_SCRATCH"`
	s := ExecSource{Name: "probe", Command: sh(script), Dir: scratch}
	evs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].ID != "env-"+execProtocolVersion {
		t.Errorf("HET_PROTOCOL not exported: id=%q", evs[0].ID)
	}
	if evs[0].Subject != scratch {
		t.Errorf("HET_SCRATCH not exported: subject=%q want %q", evs[0].Subject, scratch)
	}
	if fi, err := os.Stat(scratch); err != nil || !fi.IsDir() {
		t.Errorf("scratch dir not created: %v", err)
	}
}

// Spec Env vars are forwarded to the command.
func TestExecSource_ForwardsSpecEnv(t *testing.T) {
	s := ExecSource{
		Command: sh(`printf '[{"id":"e-%s","subject":"x"}]' "$MY_FLAG"`),
		Env:     map[string]string{"MY_FLAG": "on"},
	}
	evs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ID != "e-on" {
		t.Fatalf("spec env not forwarded: %+v", evs)
	}
}

func TestExecSource_ScratchDirCreateError(t *testing.T) {
	// A scratch dir whose parent is a regular file can't be created → fetch errors
	// before running the command.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := ExecSource{Command: sh(`echo '[]'`), Dir: filepath.Join(file, "sub")}
	if _, err := s.Fetch(context.Background()); err == nil {
		t.Fatal("want error when scratch dir cannot be created")
	}
}

func TestBuildFetch_Exec(t *testing.T) {
	// Valid exec spec compiles.
	f, err := buildFetch(SourceSpec{Type: "exec", Name: "s", Command: sh(`echo '[]'`)}, t.TempDir())
	if err != nil {
		t.Fatalf("valid exec: %v", err)
	}
	if f == nil {
		t.Fatal("nil fetch for valid exec")
	}
	// Missing command is rejected.
	if _, err := buildFetch(SourceSpec{Type: "exec", Name: "s"}, t.TempDir()); err == nil {
		t.Error("exec without command should error")
	}
}

// buildFetch wires the scratch dir under <base>/eventbus/scratch/<name>, and the
// resulting FetchFunc runs end to end.
func TestBuildFetch_ExecScratchDirWired(t *testing.T) {
	base := t.TempDir()
	spec := SourceSpec{
		Type:    "exec",
		Name:    "cursor",
		Command: sh(`printf '[{"id":"s-%s","subject":"x"}]' "$HET_SCRATCH"`),
	}
	fetch, err := buildFetch(spec, base)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "eventbus", "scratch", "cursor")
	if len(evs) != 1 || evs[0].ID != "s-"+want {
		t.Fatalf("scratch dir not wired: %+v (want suffix %q)", evs, want)
	}
}

// The registry starts and stops an exec source like any other type, persisting
// it across the lifecycle.
func TestRegistry_ExecSourceLifecycle(t *testing.T) {
	bus := New(newFake(), t.TempDir(), 8)
	reg, _ := NewRegistry(context.Background(), bus, t.TempDir())
	spec := SourceSpec{
		Name:     "s1",
		Type:     "exec",
		Interval: "1h",
		Command:  sh(`echo '[]'`),
	}
	if err := reg.AddSource(spec); err != nil {
		t.Fatal(err)
	}
	if len(reg.ListSources()) != 1 {
		t.Fatalf("want 1 source")
	}
	if err := reg.AddSource(spec); err == nil {
		t.Error("duplicate source name should error")
	}
	// A spec missing command is rejected and changes nothing.
	if err := reg.AddSource(SourceSpec{Name: "bad", Type: "exec"}); err == nil {
		t.Error("exec source without command should error")
	}
	ok, err := reg.RemoveSource("s1")
	if err != nil || !ok {
		t.Fatalf("remove source: ok=%v err=%v", ok, err)
	}
	if len(reg.ListSources()) != 0 {
		t.Error("source not removed")
	}
}

// End to end through the bus: an exec plugin's event routes to a handler and
// drives an agent turn — no recompile, just a command + a subscription.
func TestExecSource_DispatchesThroughBus(t *testing.T) {
	f := newFake()
	bus := New(f, t.TempDir(), 8)
	if err := bus.Register(Subscription{
		Name:   "h",
		Match:  func(e Event) bool { return e.Type == "pr.comment" },
		Policy: Stateless{Agent: AgentRef{ID: "coder"}, Prompt: func(e Event) string { return "handle " + e.Subject }},
	}); err != nil {
		t.Fatal(err)
	}
	fetch, err := buildFetch(SourceSpec{
		Type:      "exec",
		Name:      "pr-comments",
		Command:   sh(`echo '[{"id":"c-1","type":"pr.comment","subject":"3196"}]'`),
		EventType: "pr.comment",
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	evs, err := fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		bus.Dispatch(e)
	}
	if f.sentCount() != 1 {
		t.Fatalf("want 1 dispatched turn, got %d", f.sentCount())
	}
	if got := f.lastSent().prompt; got != "handle 3196" {
		t.Errorf("prompt = %q", got)
	}
}
