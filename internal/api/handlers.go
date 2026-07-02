package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/cma"
	"github.com/wu8685/cma-service/internal/store"
	"github.com/wu8685/cma-service/internal/translate"
)

// ----- Agents -----

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var req cma.AgentCreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	if req.Name == "" || req.Model.ID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "name and model are required")
		return
	}
	now := time.Now().UTC()
	a := &cma.Agent{
		Type: "agent", ID: cma.NewID(cma.PrefixAgent), Version: 1,
		Name: req.Name, Model: req.Model, System: req.System, Description: req.Description,
		Tools: req.Tools, Skills: req.Skills, MCPServers: req.MCPServers, Metadata: req.Metadata,
		CreatedAt: now, UpdatedAt: now,
	}
	normalizeAgent(a)
	if err := s.store.PutAgentVersion(a); err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.store.AgentRecord(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "agent not found")
		return
	}
	var req cma.AgentCreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	now := time.Now().UTC()
	a := &cma.Agent{
		Type: "agent", ID: id, Version: rec.Latest + 1,
		Name: req.Name, Model: req.Model, System: req.System, Description: req.Description,
		Tools: req.Tools, Skills: req.Skills, MCPServers: req.MCPServers, Metadata: req.Metadata,
		CreatedAt: now, UpdatedAt: now,
	}
	if a.Name == "" {
		a.Name = rec.Versions[rec.Latest].Name
	}
	normalizeAgent(a)
	if err := s.store.PutAgentVersion(a); err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	var version int64
	if v := r.URL.Query().Get("version"); v != "" {
		version, _ = strconv.ParseInt(v, 10, 64)
	}
	a, ok := s.store.Agent(r.PathValue("id"), version)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "agent not found")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, listPage(r, s.store.ListAgents(),
		func(a *cma.Agent) string { return a.ID },
		func(a *cma.Agent) time.Time { return a.CreatedAt },
		func(a *cma.Agent) bool { return a.ArchivedAt != nil }))
}

// listPage applies the SDK list contract to an already-materialized slice:
// include_archived (default exclude), order asc|desc by created_at (default
// asc; only sessions sends order), and limit + page cursor pagination (the
// opaque cursor is the item id). The input slice is a store-owned copy, so we
// sort/slice it in place. A deterministic order (created_at, then id) is
// required for stable cursor paging — the store's map iteration is random.
func listPage[T any](r *http.Request, items []T, id func(T) string, createdAt func(T) time.Time, archived func(T) bool) cma.List[T] {
	if r.URL.Query().Get("include_archived") != "true" {
		kept := make([]T, 0, len(items))
		for _, it := range items {
			if !archived(it) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	sort.SliceStable(items, func(i, j int) bool {
		ai, aj := createdAt(items[i]), createdAt(items[j])
		if ai.Equal(aj) {
			return id(items[i]) < id(items[j])
		}
		return ai.Before(aj)
	})
	if r.URL.Query().Get("order") == "desc" {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	if cur := r.URL.Query().Get("page"); cur != "" {
		start := len(items)
		for i, it := range items {
			if id(it) == cur {
				start = i + 1 // last occurrence wins → cursor always advances
			}
		}
		items = items[start:]
	}
	var next *string
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err := strconv.Atoi(v); err == nil && limit > 0 && limit < len(items) {
			last := id(items[limit-1])
			items = items[:limit]
			next = &last
		}
	}
	return cma.List[T]{Data: items, NextPage: next}
}

func (s *Server) archiveAgent(w http.ResponseWriter, r *http.Request) {
	// Archive marks every version archived; the agent stays retrievable. No
	// unarchive semantics yet.
	a, ok := s.store.ArchiveAgent(r.PathValue("id"), time.Now().UTC())
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "agent not found")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// ----- Environments -----

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var req cma.EnvironmentCreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "name is required")
		return
	}
	meta := req.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	now := time.Now().UTC()
	e := &cma.Environment{
		Type: "environment", ID: cma.NewID(cma.PrefixEnvironment),
		Name: req.Name, Description: req.Description, Config: req.Config, Metadata: meta,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.PutEnvironment(e); err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	e, ok := s.store.Environment(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "environment not found")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, listPage(r, s.store.ListEnvironments(),
		func(e *cma.Environment) string { return e.ID },
		func(e *cma.Environment) time.Time { return e.CreatedAt },
		func(e *cma.Environment) bool { return e.ArchivedAt != nil }))
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) {
	e, ok := s.store.Environment(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "environment not found")
		return
	}
	var req cma.EnvironmentUpdateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	if req.Name != nil {
		e.Name = *req.Name
	}
	if req.Description != nil {
		e.Description = *req.Description
	}
	if len(req.Config) > 0 {
		e.Config = req.Config
	}
	if req.Metadata != nil {
		e.Metadata = req.Metadata
	}
	e.UpdatedAt = time.Now().UTC()
	if err := s.store.PutEnvironment(e); err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) archiveEnvironment(w http.ResponseWriter, r *http.Request) {
	e, ok := s.store.Environment(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "environment not found")
		return
	}
	now := time.Now().UTC()
	e.ArchivedAt = &now
	e.UpdatedAt = now
	if err := s.store.PutEnvironment(e); err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.store.DeleteEnvironment(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "environment not found")
		return
	}
	writeJSON(w, http.StatusOK, cma.DeletedResource{ID: id, Type: "environment_deleted"})
}

// ----- Sessions -----

// Session-creation classified errors (mapped to HTTP by the handler; returned
// as-is to in-process callers like the event bus driver).
var (
	errAgentNotFound = errors.New("agent (or version) not found")
	errAgentArchived = errors.New("agent is archived")
	errEnvNotFound   = errors.New("environment not found")
	errEnvArchived   = errors.New("environment is archived")
)

// createSessionRecord resolves the agent + environment, registers the ahsir
// agent, and persists a new session. Shared by the HTTP handler and in-process
// callers (the event bus). Returns a classified error.
func (s *Server) createSessionRecord(ctx context.Context, ref cma.AgentRef, envID, title string, meta map[string]string) (*store.SessionRecord, error) {
	agent, ok := s.store.Agent(ref.ID, ref.Version)
	if !ok {
		return nil, errAgentNotFound
	}
	if agent.ArchivedAt != nil {
		return nil, errAgentArchived
	}
	env, ok := s.store.Environment(envID)
	if !ok {
		return nil, errEnvNotFound
	}
	if env.ArchivedAt != nil {
		return nil, errEnvArchived
	}
	ahsirName := translate.AhsirAgentName(agent.ID, agent.Version)
	if err := s.ensureRegistered(ctx, ahsirName, agent); err != nil {
		return nil, fmt.Errorf("register ahsir agent: %w", err)
	}
	now := time.Now().UTC()
	if meta == nil {
		meta = map[string]string{}
	}
	rec := &store.SessionRecord{
		Session: &cma.Session{
			Type: "session", ID: cma.NewID(cma.PrefixSession), Title: title,
			Status: cma.StatusIdle, EnvironmentID: envID,
			Agent:     sessionAgentFrom(agent),
			Resources: []any{}, VaultIDs: []string{},
			Metadata: meta, CreatedAt: now, UpdatedAt: now,
		},
		AhsirName: ahsirName,
		ContextID: cma.NewID("ctx"),
	}
	if err := s.store.PutSession(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req cma.SessionCreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	if req.Agent.ID == "" || req.EnvironmentID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "agent and environment_id are required")
		return
	}
	rec, err := s.createSessionRecord(r.Context(), req.Agent, req.EnvironmentID, req.Title, req.Metadata)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, rec.Session)
	case errors.Is(err, errAgentNotFound) || errors.Is(err, errEnvNotFound):
		writeErr(w, http.StatusNotFound, "not_found_error", err.Error())
	case errors.Is(err, errAgentArchived) || errors.Is(err, errEnvArchived):
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
	case strings.HasPrefix(err.Error(), "register ahsir agent"):
		writeErr(w, http.StatusBadGateway, "api_error", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
	}
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	writeJSON(w, http.StatusOK, rec.Session)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.store.ListSessions()
	// SDK-specific filters: agent_id / agent_version.
	if aid := r.URL.Query().Get("agent_id"); aid != "" {
		kept := sessions[:0:0]
		for _, sess := range sessions {
			if sess.Agent.ID == aid {
				kept = append(kept, sess)
			}
		}
		sessions = kept
	}
	if av := r.URL.Query().Get("agent_version"); av != "" {
		if ver, err := strconv.ParseInt(av, 10, 64); err == nil {
			kept := sessions[:0:0]
			for _, sess := range sessions {
				if sess.Agent.Version == ver {
					kept = append(kept, sess)
				}
			}
			sessions = kept
		}
	}
	writeJSON(w, http.StatusOK, listPage(r, sessions,
		func(s *cma.Session) string { return s.ID },
		func(s *cma.Session) time.Time { return s.CreatedAt },
		func(s *cma.Session) bool { return s.ArchivedAt != nil }))
}

func (s *Server) archiveSession(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	_ = s.store.ArchiveSession(rec, time.Now().UTC())
	s.gcAhsirAgentIfUnused(rec.AhsirName)
	writeJSON(w, http.StatusOK, rec.Session)
}

// gcAhsirAgentIfUnused reclaims the backing ahsir agent only when no other live
// session pins the same (agent_id, version). The agent is shared, so a refcount
// of zero is the safe condition to stop it; a future session for that version
// re-registers it via ensureRegistered (we clear the registered flag here).
func (s *Server) gcAhsirAgentIfUnused(ahsirName string) {
	if s.store.ActiveAhsirRefs(ahsirName) != 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.ahsir.DeleteAgent(ctx, ahsirName); err == nil {
		s.regMu.Lock()
		delete(s.registered, ahsirName)
		s.regMu.Unlock()
	}
}

// cancelInFlight best-effort cancels a session's running turn (A2A tasks/cancel),
// shared by user.interrupt and session delete.
func (s *Server) cancelInFlight(ctx context.Context, rec *store.SessionRecord) {
	if taskID := rec.InFlightTask(); taskID != "" {
		_ = s.ahsir.CancelTask(ctx, rec.AhsirName, taskID)
	}
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	var req cma.SessionUpdateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	_ = s.store.UpdateSessionMeta(rec, req.Title, req.Metadata, req.VaultIDs)
	writeJSON(w, http.StatusOK, rec.Session)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	// Stop any running turn and wait for it to settle before reclaiming, so we
	// don't GC the ahsir agent out from under an in-flight stream. Then tell
	// live subscribers, remove, and reclaim.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s.cancelInFlight(ctx, rec)
	waitInFlightCleared(rec, 5*time.Second)
	s.emit(rec, newEvent(cma.EvtSessionDeleted))
	_, _ = s.store.DeleteSession(rec.Session.ID)
	s.gcAhsirAgentIfUnused(rec.AhsirName)
	writeJSON(w, http.StatusOK, cma.DeletedResource{ID: rec.Session.ID, Type: "session_deleted"})
}

// waitInFlightCleared blocks until the session has no running turn (the
// cancelled turn cleared its in-flight task id) or the deadline elapses.
func waitInFlightCleared(rec *store.SessionRecord, d time.Duration) {
	deadline := time.Now().Add(d)
	for rec.InFlightTask() != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

// ----- Events -----

func (s *Server) sendEvents(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	var req cma.SendEventsRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	for _, ev := range req.Events {
		switch ev.Type {
		case cma.EvtUserMessage:
			s.deliverUserMessage(rec, ev.Content)
		case cma.EvtUserInterrupt:
			// Cancel the in-flight turn out of band (interrupt is not itself a
			// queued turn). The running executeTurn observes the cancellation
			// via its A2A stream and settles the session back to idle.
			if taskID := rec.InFlightTask(); taskID != "" {
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				err := s.ahsir.CancelTask(ctx, rec.AhsirName, taskID)
				cancel()
				log.Printf("interrupt: session=%s agent=%s task=%s cancel_err=%v", rec.Session.ID, rec.AhsirName, taskID, err)
			} else {
				log.Printf("interrupt: session=%s no in-flight task to cancel", rec.Session.ID)
			}
		default:
			// custom_tool_result / tool_confirmation deferred.
		}
	}
	// SendSessionEvents.data is optional; return an empty object.
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	// The event log is stored chronologically (asc). order=desc reverses it.
	// The cursor (`page`) is an opaque event ID; the next page starts after it.
	// limit truncates and yields a next_page cursor; absent limit returns the
	// remaining log with no cursor (the SDK's no-arg list() expects everything).
	events := rec.Snapshot()
	if r.URL.Query().Get("order") == "desc" {
		events = reverseEvents(events)
	}
	if cur := r.URL.Query().Get("page"); cur != "" {
		start := len(events) // unknown cursor → empty page (consumed past the end)
		// Last occurrence wins: if ids ever collide, the cursor still advances
		// past all copies, so client auto-pagination can't loop forever.
		for i, ev := range events {
			if ev.ID == cur {
				start = i + 1
			}
		}
		events = events[start:]
	}
	var next *string
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err := strconv.Atoi(v); err == nil && limit > 0 && limit < len(events) {
			last := events[limit-1].ID
			events = events[:limit]
			next = &last
		}
	}
	writeJSON(w, http.StatusOK, cma.List[cma.Event]{Data: events, NextPage: next})
}

// reverseEvents returns a new slice with the events in reverse order.
func reverseEvents(in []cma.Event) []cma.Event {
	out := make([]cma.Event, len(in))
	for i, ev := range in {
		out[len(in)-1-i] = ev
	}
	return out
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}
	// Subscribe BEFORE writing headers so that once the client's stream() call
	// returns (headers received), the subscription is already live and no event
	// sent afterwards can be missed. Per CMA semantics the stream is a live tail
	// from connect onward — it does NOT replay history (use events.list for that).
	_, ch, cancel := rec.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			writeSSE(w, ev)
			flusher.Flush()
		case <-ping.C:
			// SSE comment heartbeat keeps proxies from idling the stream out.
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev cma.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("event: " + ev.Type + "\ndata: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
}

// ----- turn execution -----

// runTurn enqueues one user message as a turn. Turns for a session run strictly
// FIFO and never interleave (EnqueueTurn serializes them on a per-session drain
// goroutine), so the status_running → agent.message → status_idle sequence of
// one turn always completes before the next begins.
func (s *Server) runTurn(rec *store.SessionRecord, text string) {
	rec.EnqueueTurn(func() { s.executeTurn(rec, text) })
}

// executeTurn drives one user message through ahsir and emits CMA events. Runs
// on the session's serial turn goroutine, so it may block on ahsir.
//
// The turn is driven over the A2A message/stream transport: text deltas are
// accumulated and emitted as a single agent.message at completion (the CMA
// agent.message event carries a complete message, not deltas). The A2A task id
// is published to the record as soon as it's known so a concurrent
// user.interrupt can cancel this turn (→ tasks/cancel).
func (s *Server) executeTurn(rec *store.SessionRecord, text string) {
	s.setStatus(rec, cma.StatusRunning)
	s.emit(rec, newEvent(cma.EvtSessionStatusRunning))

	turnTimeout := s.cfg.TurnTimeout
	if turnTimeout <= 0 {
		turnTimeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	defer cancel()

	onReschedule := func() { s.emit(rec, newEvent(cma.EvtSessionStatusRescheduled)) }
	// onEvent maps ahsir's structured stream events (DataParts) to CMA
	// observability events, emitted live as the agent works (before the final
	// agent.message). obs holds per-turn linking state; the callback runs on
	// this turn's goroutine, so it's race-free.
	obs := &turnObs{mcpToolUse: map[string]bool{}}
	onEvent := func(se ahsir.StreamEvent) { s.emitObservability(rec, se, obs) }
	reply, err := s.ahsir.ChatStream(ctx, rec.AhsirName, rec.ContextID, text, rec.SetInFlightTask, onReschedule, onEvent)
	rec.SetInFlightTask("") // turn is no longer cancelable

	switch {
	case errors.Is(err, ahsir.ErrTurnCanceled):
		// Interrupted: surface any partial text, then return to idle (ready for
		// the next turn) rather than terminating the session.
		s.emitMessageIfAny(rec, reply)
		s.emitIdle(rec)
	case err != nil:
		e := newEvent(cma.EvtSessionError)
		e.Error = &cma.EventError{Type: "unknown_error", Message: err.Error(), RetryStatus: cma.RetryStatus{Type: "terminal"}}
		s.emit(rec, e)
		s.emit(rec, newEvent(cma.EvtSessionStatusTerminate))
		s.setStatus(rec, cma.StatusTerminated)
	default:
		msg := newEvent(cma.EvtAgentMessage)
		msg.Content = []cma.ContentBlock{{Type: "text", Text: reply}}
		s.emit(rec, msg)
		s.emitIdle(rec)
	}
}

func (s *Server) emitMessageIfAny(rec *store.SessionRecord, text string) {
	if text == "" {
		return
	}
	msg := newEvent(cma.EvtAgentMessage)
	msg.Content = []cma.ContentBlock{{Type: "text", Text: text}}
	s.emit(rec, msg)
}

func (s *Server) emitIdle(rec *store.SessionRecord) {
	idle := newEvent(cma.EvtSessionStatusIdle)
	idle.StopReason = &cma.StopReason{Type: "end_turn"}
	s.emit(rec, idle)
	s.setStatus(rec, cma.StatusIdle)
}

// emit appends an event to the session log (persisting it immediately) and fans
// it out to live stream subscribers.
func (s *Server) emit(rec *store.SessionRecord, ev cma.Event) {
	_ = s.store.AppendEvent(rec, ev)
}

// turnObs is per-turn observability linking state, threaded through one turn's
// emitObservability calls (which run serially on the turn goroutine).
type turnObs struct {
	spanStartID string          // most recent span_start event id, for span_end linking
	mcpToolUse  map[string]bool // tool_use ids that were MCP tools, for result classification
}

// emitObservability maps one ahsir StreamEvent to its CMA observability event.
// tool_use is classified as mcp_tool_use when the runtime tool name carries the
// claude MCP prefix (mcp__<server>__<tool>); a tool_result inherits that
// classification via its tool_use id.
func (s *Server) emitObservability(rec *store.SessionRecord, se ahsir.StreamEvent, obs *turnObs) {
	switch se.Kind {
	case "tool_use":
		input := se.Input
		if len(input) == 0 {
			input = json.RawMessage("{}") // agent.tool_use.input is required
		}
		ev := newEvent(cma.EvtAgentToolUse)
		// The CMA tool_use event id IS the tool-use identifier that a later
		// tool_result references — use the runtime's id when present.
		if se.ID != "" {
			ev.ID = se.ID
		}
		ev.Input = input
		if server, tool, ok := splitMCPToolName(se.Name); ok {
			ev.Type = cma.EvtAgentMCPToolUse
			ev.MCPServerName = server
			ev.Name = tool
			obs.mcpToolUse[ev.ID] = true
		} else {
			ev.Name = se.Name
		}
		s.emit(rec, ev)
	case "thinking":
		s.emit(rec, newEvent(cma.EvtAgentThinking))
	case "tool_result":
		ev := newEvent(cma.EvtAgentToolResult)
		if se.Content != "" {
			ev.Content = []cma.ContentBlock{{Type: "text", Text: se.Content}}
		}
		ev.IsError = se.IsError
		if obs.mcpToolUse[se.ToolUseID] {
			ev.Type = cma.EvtAgentMCPToolResult
			ev.MCPToolUseID = se.ToolUseID
		} else {
			ev.ToolUseID = se.ToolUseID
		}
		s.emit(rec, ev)
	case "span_start":
		ev := newEvent(cma.EvtSpanModelRequestStart)
		obs.spanStartID = ev.ID
		s.emit(rec, ev)
	case "span_end":
		ev := newEvent(cma.EvtSpanModelRequestEnd)
		ev.ModelRequestStartID = obs.spanStartID
		ev.ModelUsage = &cma.ModelUsage{}
		if se.Usage != nil {
			ev.ModelUsage.InputTokens = se.Usage.InputTokens
			ev.ModelUsage.OutputTokens = se.Usage.OutputTokens
		}
		s.emit(rec, ev)
	}
}

// splitMCPToolName recognizes claude's MCP tool naming, mcp__<server>__<tool>,
// returning (server, tool, true) for MCP tools and ("","",false) otherwise.
func splitMCPToolName(name string) (string, string, bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := name[len(prefix):]
	i := strings.Index(rest, "__")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+2:], true
}

func (s *Server) setStatus(rec *store.SessionRecord, status string) {
	_ = s.store.SetSessionStatus(rec, status, time.Now().UTC())
}

// ensureRegistered registers the versioned ahsir agent once per process.
func (s *Server) ensureRegistered(ctx context.Context, ahsirName string, agent *cma.Agent) error {
	s.regMu.Lock()
	already := s.registered[ahsirName]
	s.regMu.Unlock()
	if already {
		return nil
	}
	card := translate.AgentToCard(ahsirName, agent, s.rt)
	if err := s.ahsir.RegisterAgent(ctx, ahsirName, card); err != nil {
		return err
	}
	s.regMu.Lock()
	s.registered[ahsirName] = true
	s.regMu.Unlock()
	return nil
}

func textOf(blocks []cma.ContentBlock) string {
	out := ""
	for _, b := range blocks {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}

// normalizeAgent ensures the SDK-required list/map fields serialize as [] / {}
// rather than null.
func normalizeAgent(a *cma.Agent) {
	if a.Tools == nil {
		a.Tools = []cma.ToolDef{}
	}
	if a.Skills == nil {
		a.Skills = []cma.SkillRef{}
	}
	if a.MCPServers == nil {
		a.MCPServers = []cma.MCPServer{}
	}
	if a.Metadata == nil {
		a.Metadata = map[string]string{}
	}
}

// sessionAgentFrom builds the embedded full-agent snapshot for a session.
func sessionAgentFrom(a *cma.Agent) cma.SessionAgent {
	sa := cma.SessionAgent{
		Type: "agent", ID: a.ID, Version: a.Version, Name: a.Name,
		Model: a.Model, System: a.System, Description: a.Description,
		Tools: a.Tools, Skills: a.Skills, MCPServers: a.MCPServers,
	}
	if sa.Tools == nil {
		sa.Tools = []cma.ToolDef{}
	}
	if sa.Skills == nil {
		sa.Skills = []cma.SkillRef{}
	}
	if sa.MCPServers == nil {
		sa.MCPServers = []cma.MCPServer{}
	}
	return sa
}
