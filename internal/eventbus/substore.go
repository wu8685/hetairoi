package eventbus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// subStore holds one subscription's persistent state: Keyed bindings, the list
// of sessions it created (Routed candidates), and the rotating dedup window.
// One JSON file per subscription under <dir>/eventbus/<name>.json.
type subStore struct {
	path    string
	maxSeen int
	ttl     time.Duration

	mu    sync.Mutex
	state subState
	set   map[string]struct{} // index over state.Seen for O(1) dedup
}

type subState struct {
	Bindings map[string]string `json:"bindings"` // Keyed: key → sessionID
	Created  []string          `json:"created"`  // Routed: sessionIDs created, oldest first
	Seen     []seenEntry       `json:"seen"`     // dedup window, oldest first
}

type seenEntry struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
}

func newSubStore(dir, name string, cfg DedupConfig) (*subStore, error) {
	max := cfg.MaxEntries
	if max <= 0 {
		max = defaultDedupMaxEntries
	}
	s := &subStore{
		path:    filepath.Join(dir, "eventbus", name+".json"),
		maxSeen: max,
		ttl:     cfg.TTL,
		state:   subState{Bindings: map[string]string{}},
		set:     map[string]struct{}{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *subStore) load() error {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		return err
	}
	if s.state.Bindings == nil {
		s.state.Bindings = map[string]string{}
	}
	for _, e := range s.state.Seen {
		s.set[e.ID] = struct{}{}
	}
	return nil
}

// persist must be called with s.mu held.
func (s *subStore) persist() error {
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// markSeen records an event id and reports whether it is NEW (true → process it;
// false → duplicate within the window, skip). Expired/overflow ids rotate out,
// so an id seen again after its window can process once more (at-least-once).
func (s *subStore) markSeen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpired()
	if _, ok := s.set[id]; ok {
		return false
	}
	s.set[id] = struct{}{}
	s.state.Seen = append(s.state.Seen, seenEntry{ID: id, Time: time.Now()})
	s.evictOverflow()
	_ = s.persist()
	return true
}

func (s *subStore) evictExpired() {
	if s.ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-s.ttl)
	i := 0
	for i < len(s.state.Seen) && s.state.Seen[i].Time.Before(cutoff) {
		delete(s.set, s.state.Seen[i].ID)
		i++
	}
	if i > 0 {
		s.state.Seen = append([]seenEntry(nil), s.state.Seen[i:]...)
	}
}

func (s *subStore) evictOverflow() {
	for len(s.state.Seen) > s.maxSeen {
		delete(s.set, s.state.Seen[0].ID)
		s.state.Seen = s.state.Seen[1:]
	}
}

// ----- Keyed bindings -----

func (s *subStore) binding(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sid, ok := s.state.Bindings[key]
	return sid, ok
}

func (s *subStore) bind(key, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Bindings[key] = sessionID
	_ = s.persist()
}

// ----- Routed created list -----

func (s *subStore) addCreated(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Created = append(s.state.Created, sessionID)
	_ = s.persist()
}

// recentCreated returns up to n session ids, most-recently-created first.
func (s *subStore) recentCreated(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, n)
	for i := len(s.state.Created) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.state.Created[i])
	}
	return out
}
