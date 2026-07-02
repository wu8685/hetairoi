package api

import (
	"net/http"

	"github.com/wu8685/cma-service/internal/eventbus"
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
	// Validate the referenced agent exists, so a typo fails loudly at create
	// time rather than at first dispatch.
	if _, ok := s.store.Agent(spec.Policy.AgentID, spec.Policy.Version); !ok {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "policy.agent_id not found: "+spec.Policy.AgentID)
		return
	}
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
