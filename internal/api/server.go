// Package api exposes the CMA-compatible HTTP surface and drives ahsir behind it.
package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/cma"
	"github.com/wu8685/cma-service/internal/config"
	"github.com/wu8685/cma-service/internal/eventbus"
	"github.com/wu8685/cma-service/internal/store"
	"github.com/wu8685/cma-service/internal/translate"
)

type Server struct {
	cfg   config.Config
	store *store.Store
	ahsir *ahsir.Client
	rt    translate.RuntimeDefaults

	regMu      sync.Mutex
	registered map[string]bool // ahsir agent name -> registered this process

	eventBus *eventbus.Bus      // optional; mounts the webhook when set
	eventReg *eventbus.Registry // optional; mounts the dynamic sources/handlers API

	// externalAgents = agents live outside this process's store (SDK-driver
	// mode: the eventbus drives an external CMA facade). When set, handler
	// creation skips the local agent-existence check.
	externalAgents bool
}

// SetExternalAgents marks that agents are owned by an external CMA facade (not
// this Server's store), so eventbus handler creation must not validate agent_id
// against the local store. Call when wiring the SDK eventbus driver.
func (s *Server) SetExternalAgents(v bool) { s.externalAgents = v }

// SetEventBus mounts an event bus, exposing its webhook at POST /eventbus/events
// (behind the same x-api-key gate as the CMA API in v1). Call before Handler().
func (s *Server) SetEventBus(b *eventbus.Bus) { s.eventBus = b }

// SetEventRegistry mounts the dynamic control plane (POST/GET/DELETE
// /v1/eventbus/{sources,handlers}) backed by reg, and its bus's webhook. Use
// this instead of SetEventBus when handlers/sources are managed at runtime.
func (s *Server) SetEventRegistry(reg *eventbus.Registry) {
	s.eventReg = reg
	s.eventBus = reg.Bus()
}

func New(cfg config.Config, st *store.Store, ac *ahsir.Client) *Server {
	return &Server{
		cfg:        cfg,
		store:      st,
		ahsir:      ac,
		rt:         translate.RuntimeDefaults{Provider: cfg.RuntimeProvider, BaseURL: cfg.RuntimeBaseURL, APIKey: cfg.RuntimeAPIKey},
		registered: map[string]bool{},
	}
}

// Handler builds the routed, authenticated http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/agents", s.createAgent)
	mux.HandleFunc("GET /v1/agents", s.listAgents)
	mux.HandleFunc("GET /v1/agents/{id}", s.getAgent)
	mux.HandleFunc("POST /v1/agents/{id}", s.updateAgent)
	mux.HandleFunc("POST /v1/agents/{id}/archive", s.archiveAgent)

	mux.HandleFunc("POST /v1/environments", s.createEnvironment)
	mux.HandleFunc("GET /v1/environments", s.listEnvironments)
	mux.HandleFunc("GET /v1/environments/{id}", s.getEnvironment)
	mux.HandleFunc("POST /v1/environments/{id}", s.updateEnvironment)
	mux.HandleFunc("POST /v1/environments/{id}/archive", s.archiveEnvironment)
	mux.HandleFunc("DELETE /v1/environments/{id}", s.deleteEnvironment)

	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", s.getSession)
	mux.HandleFunc("POST /v1/sessions/{id}", s.updateSession)
	mux.HandleFunc("POST /v1/sessions/{id}/archive", s.archiveSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)

	mux.HandleFunc("POST /v1/sessions/{id}/events", s.sendEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.listEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/events/stream", s.streamEvents)

	if s.eventBus != nil {
		mux.Handle("POST /eventbus/events", s.eventBus.WebhookHandler())
	}
	if s.eventReg != nil {
		mux.HandleFunc("POST /v1/eventbus/sources", s.createEventSource)
		mux.HandleFunc("GET /v1/eventbus/sources", s.listEventSources)
		mux.HandleFunc("DELETE /v1/eventbus/sources/{name}", s.deleteEventSource)
		mux.HandleFunc("POST /v1/eventbus/handlers", s.createEventHandler)
		mux.HandleFunc("GET /v1/eventbus/handlers", s.listEventHandlers)
		mux.HandleFunc("DELETE /v1/eventbus/handlers/{name}", s.deleteEventHandler)
	}

	return s.auth(mux)
}

// auth gates on x-api-key. Empty allowlist => allow all (local/zero-config).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.APIKeys) > 0 {
			key := r.Header.Get("x-api-key")
			if key == "" {
				key = r.Header.Get("anthropic-api-key")
			}
			if !s.cfg.APIKeys[key] {
				writeErr(w, http.StatusUnauthorized, "authentication_error", "invalid or missing x-api-key")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ----- helpers -----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, cma.APIError{Type: "error", Error: cma.Err{Type: typ, Message: msg}})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func newEvent(typ string) cma.Event {
	now := time.Now().UTC()
	return cma.Event{ID: cma.NewID(cma.PrefixEvent), Type: typ, ProcessedAt: &now}
}
