// Package api exposes Hetairoi's event-bus control plane over HTTP. Hetairoi is
// the event-driven scenario layer: it watches external sources and drives ahsir
// agents through the official CMA SDK (internal/sdkdriver). The CMA API itself
// lives in ahsir now — this package only serves the eventbus admin + webhook.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/wu8685/hetairoi/internal/config"
	"github.com/wu8685/hetairoi/internal/eventbus"
)

type Server struct {
	cfg config.Config

	eventBus *eventbus.Bus      // optional; mounts the webhook when set
	eventReg *eventbus.Registry // optional; mounts the dynamic sources/handlers API
}

func New(cfg config.Config) *Server { return &Server{cfg: cfg} }

// SetEventBus mounts an event bus, exposing its webhook at POST /eventbus/events
// (behind the same x-api-key gate as the control plane). Call before Handler().
func (s *Server) SetEventBus(b *eventbus.Bus) { s.eventBus = b }

// SetEventRegistry mounts the dynamic control plane (POST/GET/DELETE
// /v1/eventbus/{sources,handlers}) backed by reg, and its bus's webhook. Use
// this instead of SetEventBus when handlers/sources are managed at runtime.
func (s *Server) SetEventRegistry(reg *eventbus.Registry) {
	s.eventReg = reg
	s.eventBus = reg.Bus()
}

// Handler builds the routed, authenticated http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

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
		mux.HandleFunc("POST /v1/eventbus/handlers/{name}/retry", s.retryEventHandler)
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

// apiError is the error envelope (kept CMA-shaped for client familiarity, but
// self-contained now that the CMA wire-type package has moved into ahsir).
type apiError struct {
	Type  string  `json:"type"`
	Error errBody `json:"error"`
}

type errBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, apiError{Type: "error", Error: errBody{Type: typ, Message: msg}})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
