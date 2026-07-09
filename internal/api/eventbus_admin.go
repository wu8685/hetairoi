package api

import (
	"errors"
	"net/http"

	"github.com/wu8685/hetairoi/internal/eventbus"
)

// Event-bus control plane: create/list/delete sources and handlers at runtime.
// Mounted only when an eventbus.Registry is attached (SetEventRegistry). The
// declarations are persisted by the registry and rebuilt on the next boot.

func (s *Server) createEventSource(w http.ResponseWriter, r *http.Request) {
	var spec eventbus.SourceSpec
	if err := decode(r, &spec); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	if err := s.eventReg.AddSource(spec); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, spec)
}

func (s *Server) listEventSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.eventReg.ListSources()})
}

func (s *Server) deleteEventSource(w http.ResponseWriter, r *http.Request) {
	ok, err := s.eventReg.RemoveSource(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "source not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createEventHandler(w http.ResponseWriter, r *http.Request) {
	var spec eventbus.HandlerSpec
	if err := decode(r, &spec); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	// Agent-id validity is enforced by the CMA facade when the SDK driver creates
	// a session on it — Hetairoi no longer owns a local agent store to check.
	if err := s.eventReg.AddHandler(spec); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, spec)
}

func (s *Server) listEventHandlers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.eventReg.ListHandlers()})
}

func (s *Server) deleteEventHandler(w http.ResponseWriter, r *http.Request) {
	ok, err := s.eventReg.RemoveHandler(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found_error", "handler not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// retryRequest selects which previously-seen event to replay. Exactly one of
// key/event_id/subject identifies the event (precedence event_id → key →
// subject); fresh_session forces a new session for a Keyed handler.
type retryRequest struct {
	Key          string `json:"key,omitempty"`
	EventID      string `json:"event_id,omitempty"`
	Subject      string `json:"subject,omitempty"`
	FreshSession bool   `json:"fresh_session,omitempty"`
}

// retryEventHandler re-dispatches a stored event for a handler, bypassing dedup,
// with no change to upstream state. Response mirrors the webhook path:
// {"results": [DispatchResult…]} carrying the resolved session id(s).
func (s *Server) retryEventHandler(w http.ResponseWriter, r *http.Request) {
	var req retryRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	results, err := s.eventReg.Retry(r.PathValue("name"), eventbus.RetryTarget{
		Key:          req.Key,
		EventID:      req.EventID,
		Subject:      req.Subject,
		FreshSession: req.FreshSession,
	})
	if err != nil {
		switch {
		case errors.Is(err, eventbus.ErrHandlerNotFound), errors.Is(err, eventbus.ErrEventNotFound):
			writeErr(w, http.StatusNotFound, "not_found_error", err.Error())
		case errors.Is(err, eventbus.ErrNoRetryTarget):
			writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
