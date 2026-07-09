package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"
)

// ExecSource is the generic pluggable poll Source: instead of a compiled-in
// FetchFunc, it shells out to an operator-supplied command every interval and
// reads a JSON array of events from its stdout. This externalizes the one thing
// that used to require a code change per upstream — the fetch — while the bus
// keeps owning scheduling, dedup, routing, and persistence.
//
// The plugin owns how to talk to the upstream AND, critically, how to encode the
// item's mutable version into each Event.ID (e.g. pr-<iid>-<sha>): that is the
// whole basis of "fire once per change", since the bus dedups by ID. hetairoi
// fills Source and Time and routes the events through the normal machinery.
//
// Contract (a public interface — keep minimal, versioned via HET_PROTOCOL):
//
//	stdout: [{"id":"...","type":"...","subject":"...","payload":{...}}, ...]
//	env:    HET_PROTOCOL=1, HET_SCRATCH=<per-source scratch dir> (+ spec Env)
//
// A non-zero exit or malformed JSON is returned as a fetch error (logged by the
// Poller) and dispatches nothing — mirroring the built-in pollers.
//
// SECURITY: exec runs an arbitrary command. This is acceptable for a local,
// single-user tool behind a loopback control plane, but source specs must come
// from a trusted operator.
type ExecSource struct {
	Name      string            // source name — used for the Event.Source label
	Command   []string          // argv; Command[0] is the program, rest are args
	Env       map[string]string // extra env vars merged over the inherited environment
	Dir       string            // per-source scratch dir, exported as HET_SCRATCH
	EventType string            // default emitted Event.Type when a plugin event omits "type"
}

// execProtocolVersion is the value of HET_PROTOCOL. Bump it only on a breaking
// change to the stdout event contract so plugins can gate their output.
const execProtocolVersion = "1"

// execEvent is the wire shape hetairoi reads from a plugin's stdout. It is
// intentionally tiny: id (dedup identity), type (routing), subject (routing
// key), and an opaque payload the handlers match/template over.
type execEvent struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Subject string          `json:"subject"`
	Payload json.RawMessage `json:"payload"`
}

func (s ExecSource) sourceLabel() string {
	if s.Name != "" {
		return "exec:" + s.Name
	}
	return "exec"
}

// env builds the child process environment: the inherited environment, then the
// spec's Env (sorted for determinism), then the hetairoi-owned HET_* vars last so
// they always win.
func (s ExecSource) env() []string {
	env := os.Environ()
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+s.Env[k])
	}
	env = append(env, "HET_PROTOCOL="+execProtocolVersion)
	if s.Dir != "" {
		env = append(env, "HET_SCRATCH="+s.Dir)
	}
	return env
}

// Fetch implements FetchFunc: run the command with a scratch dir + HET_* env,
// parse its stdout as a JSON array of events, and build one Event per entry with
// Source and Time filled in. Events without an id are skipped (no stable dedup
// identity is possible, so emitting them would churn).
func (s ExecSource) Fetch(ctx context.Context) ([]Event, error) {
	if len(s.Command) == 0 {
		return nil, fmt.Errorf("exec source: empty command")
	}
	if s.Dir != "" {
		if err := os.MkdirAll(s.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("exec source %q scratch dir: %w", s.Name, err)
		}
	}

	cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
	cmd.Env = s.env()
	cmd.Dir = s.Dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("exec source %v: %w: %s", s.Command, err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("exec source %v: %w", s.Command, err)
	}

	var raw []execEvent
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("exec source %v stdout: %w", s.Command, err)
	}

	now := time.Now()
	var evs []Event
	for _, r := range raw {
		if r.ID == "" {
			continue // no stable dedup identity — skip rather than churn
		}
		typ := r.Type
		if typ == "" {
			typ = s.EventType
		}
		evs = append(evs, Event{
			ID:      r.ID,
			Type:    typ,
			Subject: r.Subject,
			Payload: r.Payload,
			Source:  s.sourceLabel(),
			Time:    now,
		})
	}
	return evs, nil
}
