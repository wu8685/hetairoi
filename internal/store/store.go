// Package store holds the CMA resource state and the session-to-ahsir mapping.
// In-memory with whole-file JSON persistence (good enough for the MVP; swap for
// a real store later). Live event subscribers are not persisted.
package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/wu8685/hetairoi/internal/cma"
)

// AgentRecord keeps every immutable version of one agent.
type AgentRecord struct {
	ID       string                `json:"id"`
	Latest   int64                 `json:"latest"`
	Versions map[int64]*cma.Agent  `json:"versions"`
}

// SessionRecord binds a CMA session to an ahsir (agent name, contextId) and
// holds its event log.
type SessionRecord struct {
	Session   *cma.Session `json:"session"`
	AhsirName string       `json:"ahsir_name"`
	ContextID string       `json:"context_id"`
	Events    []cma.Event  `json:"events"`

	mu   sync.Mutex
	subs map[chan cma.Event]struct{}

	// inFlightTaskID is the A2A task id of the turn currently executing, set
	// once the stream reports it and cleared when the turn ends. Transient
	// (never persisted) — it's only actionable within the running process and
	// backs user.interrupt → tasks/cancel.
	inFlightTaskID string

	// Serial turn executor: a session's turns must run strictly FIFO and never
	// interleave (one ahsir agent + one contextId per session). EnqueueTurn
	// appends a job and a single drain goroutine runs them one at a time.
	turnMu    sync.Mutex
	turnQueue []func()
	turnBusy  bool
}

// EnqueueTurn schedules a turn to run after all previously-enqueued turns for
// this session have completed. Returns immediately; the job runs on a private
// drain goroutine so a session's turns are serialized without blocking the
// caller (the HTTP send handler).
func (r *SessionRecord) EnqueueTurn(job func()) {
	r.turnMu.Lock()
	r.turnQueue = append(r.turnQueue, job)
	if r.turnBusy {
		r.turnMu.Unlock()
		return
	}
	r.turnBusy = true
	r.turnMu.Unlock()
	go r.drainTurns()
}

func (r *SessionRecord) drainTurns() {
	for {
		r.turnMu.Lock()
		if len(r.turnQueue) == 0 {
			r.turnBusy = false
			r.turnMu.Unlock()
			return
		}
		job := r.turnQueue[0]
		r.turnQueue = r.turnQueue[1:]
		r.turnMu.Unlock()
		job()
	}
}

// persisted is the on-disk shape.
type persisted struct {
	Agents       map[string]*AgentRecord    `json:"agents"`
	Environments map[string]*cma.Environment `json:"environments"`
	Sessions     map[string]*SessionRecord  `json:"sessions"`
}

type Store struct {
	mu           sync.RWMutex
	path         string
	agents       map[string]*AgentRecord
	environments map[string]*cma.Environment
	sessions     map[string]*SessionRecord
}

func New(path string) (*Store, error) {
	s := &Store{
		path:         path,
		agents:       map[string]*AgentRecord{},
		environments: map[string]*cma.Environment{},
		sessions:     map[string]*SessionRecord{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.Agents != nil {
		s.agents = p.Agents
	}
	if p.Environments != nil {
		s.environments = p.Environments
	}
	if p.Sessions != nil {
		s.sessions = p.Sessions
	}
	for _, rec := range s.sessions {
		rec.subs = map[chan cma.Event]struct{}{}
	}
	return nil
}

// saveLocked writes the whole store. Callers must hold s.mu (read or write).
// It snapshots each session's mutable state (Session struct + event log) under
// that record's own mu before marshalling, so a concurrent Append/status write
// on another session can't race the JSON encoder reading a slice mid-append.
func (s *Store) saveLocked() error {
	snap := persisted{
		Agents:       s.agents,
		Environments: s.environments,
		Sessions:     make(map[string]*SessionRecord, len(s.sessions)),
	}
	for id, rec := range s.sessions {
		rec.mu.Lock()
		sessCopy := *rec.Session // shallow copy; nested maps/slices are immutable post-create
		evCopy := make([]cma.Event, len(rec.Events))
		copy(evCopy, rec.Events)
		rec.mu.Unlock()
		snap.Sessions[id] = &SessionRecord{
			Session:   &sessCopy,
			AhsirName: rec.AhsirName,
			ContextID: rec.ContextID,
			Events:    evCopy,
		}
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// persist snapshots and writes the store without the caller holding s.mu. Used
// by the event-append and status-change paths that mutate a single record.
func (s *Store) persist() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveLocked()
}

// ----- Agents -----

func (s *Store) PutAgentVersion(a *cma.Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.agents[a.ID]
	if rec == nil {
		rec = &AgentRecord{ID: a.ID, Versions: map[int64]*cma.Agent{}}
		s.agents[a.ID] = rec
	}
	rec.Versions[a.Version] = a
	if a.Version > rec.Latest {
		rec.Latest = a.Version
	}
	return s.saveLocked()
}

// Agent returns a specific version, or the latest when version == 0.
func (s *Store) Agent(id string, version int64) (*cma.Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec := s.agents[id]
	if rec == nil {
		return nil, false
	}
	if version == 0 {
		version = rec.Latest
	}
	a, ok := rec.Versions[version]
	return a, ok
}

func (s *Store) AgentRecord(id string) (*AgentRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.agents[id]
	return rec, ok
}

// ArchiveAgent marks every version of an agent archived (so retrieve of any
// version reflects it, not just the latest) and returns the latest version.
func (s *Store) ArchiveAgent(id string, at time.Time) (*cma.Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.agents[id]
	if rec == nil {
		return nil, false
	}
	for _, a := range rec.Versions {
		a.ArchivedAt = &at
		a.UpdatedAt = at
	}
	latest := rec.Versions[rec.Latest]
	_ = s.saveLocked()
	return latest, true
}

func (s *Store) ListAgents() []*cma.Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*cma.Agent, 0, len(s.agents))
	for _, rec := range s.agents {
		if a, ok := rec.Versions[rec.Latest]; ok {
			out = append(out, a)
		}
	}
	return out
}

// ----- Environments -----

func (s *Store) PutEnvironment(e *cma.Environment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.environments[e.ID] = e
	return s.saveLocked()
}

func (s *Store) Environment(id string) (*cma.Environment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.environments[id]
	return e, ok
}

// DeleteEnvironment removes an environment. Returns false if it didn't exist.
func (s *Store) DeleteEnvironment(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.environments[id]; !ok {
		return false, nil
	}
	delete(s.environments, id)
	return true, s.saveLocked()
}

func (s *Store) ListEnvironments() []*cma.Environment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*cma.Environment, 0, len(s.environments))
	for _, e := range s.environments {
		out = append(out, e)
	}
	return out
}

// ----- Sessions -----

func (s *Store) PutSession(rec *SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.subs == nil {
		rec.subs = map[chan cma.Event]struct{}{}
	}
	s.sessions[rec.Session.ID] = rec
	return s.saveLocked()
}

func (s *Store) Session(id string) (*SessionRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.sessions[id]
	return rec, ok
}

// DeleteSession removes a session. Returns false if it didn't exist.
func (s *Store) DeleteSession(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return false, nil
	}
	delete(s.sessions, id)
	return true, s.saveLocked()
}

// ActiveAhsirRefs counts non-archived sessions bound to the given ahsir agent
// name. An ahsir agent is shared across all sessions pinning the same
// (agent_id, version), so it may only be reclaimed when this returns 0.
func (s *Store) ActiveAhsirRefs(ahsirName string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, rec := range s.sessions {
		if rec.AhsirName != ahsirName {
			continue
		}
		rec.mu.Lock()
		archived := rec.Session.ArchivedAt != nil
		rec.mu.Unlock()
		if !archived {
			n++
		}
	}
	return n
}

func (s *Store) ListSessions() []*cma.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*cma.Session, 0, len(s.sessions))
	for _, rec := range s.sessions {
		out = append(out, rec.Session)
	}
	return out
}

// SaveSnapshot persists current state (call after mutating a session in place).
func (s *Store) SaveSnapshot() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveLocked()
}

// AppendEvent appends an event to a session's log, fans it out to live
// subscribers, and persists immediately so the log survives a crash mid-turn.
func (s *Store) AppendEvent(rec *SessionRecord, ev cma.Event) error {
	rec.Append(ev)
	return s.persist()
}

// SetSessionStatus updates a session's status (under the record's lock) and
// persists. Returns the error from persistence, if any.
func (s *Store) SetSessionStatus(rec *SessionRecord, status string, updatedAt time.Time) error {
	rec.mu.Lock()
	rec.Session.Status = status
	rec.Session.UpdatedAt = updatedAt
	rec.mu.Unlock()
	return s.persist()
}

// UpdateSessionMeta applies a partial metadata update (title / metadata /
// vault_ids — only non-nil fields) under the record's lock and persists.
func (s *Store) UpdateSessionMeta(rec *SessionRecord, title *string, metadata map[string]string, vaultIDs []string) error {
	rec.mu.Lock()
	if title != nil {
		rec.Session.Title = *title
	}
	if metadata != nil {
		rec.Session.Metadata = metadata
	}
	if vaultIDs != nil {
		rec.Session.VaultIDs = vaultIDs
	}
	rec.Session.UpdatedAt = time.Now().UTC()
	rec.mu.Unlock()
	return s.persist()
}

// ArchiveSession marks a session archived + terminated (under the record's
// lock) and persists.
func (s *Store) ArchiveSession(rec *SessionRecord, at time.Time) error {
	rec.mu.Lock()
	rec.Session.ArchivedAt = &at
	rec.Session.Status = cma.StatusTerminated
	rec.mu.Unlock()
	return s.persist()
}

// ----- Event log + live subscription on a SessionRecord -----

// Append adds an event to the session log and fans it out to live subscribers.
func (r *SessionRecord) Append(ev cma.Event) {
	r.mu.Lock()
	r.Events = append(r.Events, ev)
	for ch := range r.subs {
		select {
		case ch <- ev:
		default: // slow consumer — drop; it can recover via the event list
		}
	}
	r.mu.Unlock()
}

// SetInFlightTask records (or clears, with "") the A2A task id of the turn
// currently running for this session.
func (r *SessionRecord) SetInFlightTask(id string) {
	r.mu.Lock()
	r.inFlightTaskID = id
	r.mu.Unlock()
}

// InFlightTask returns the A2A task id of the running turn, or "" if idle.
func (r *SessionRecord) InFlightTask() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlightTaskID
}

// Snapshot returns a copy of the current event log.
func (r *SessionRecord) Snapshot() []cma.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]cma.Event, len(r.Events))
	copy(out, r.Events)
	return out
}

// Subscribe returns the current event log plus a channel of subsequent events.
// Call the returned cancel func to unsubscribe.
func (r *SessionRecord) Subscribe() (backlog []cma.Event, ch chan cma.Event, cancel func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	backlog = make([]cma.Event, len(r.Events))
	copy(backlog, r.Events)
	ch = make(chan cma.Event, 64)
	if r.subs == nil {
		r.subs = map[chan cma.Event]struct{}{}
	}
	r.subs[ch] = struct{}{}
	cancel = func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
	return backlog, ch, cancel
}
