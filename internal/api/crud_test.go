package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wu8685/hetairoi/internal/ahsir"
	"github.com/wu8685/hetairoi/internal/cma"
	"github.com/wu8685/hetairoi/internal/store"
)

func newServer(t *testing.T, ahsirURL string) *Server {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: st, ahsir: ahsir.New(ahsirURL, ""), registered: map[string]bool{}}
}

func TestUpdateEnvironment_Partial(t *testing.T) {
	s := newServer(t, "")
	now := time.Now().UTC()
	_ = s.store.PutEnvironment(&cma.Environment{
		Type: "environment", ID: "env_1", Name: "orig", Description: "d",
		Metadata: map[string]string{"keep": "1"}, CreatedAt: now, UpdatedAt: now,
	})

	// Only name + metadata provided — description must be left intact.
	req := httptest.NewRequest(http.MethodPost, "/v1/environments/env_1",
		strings.NewReader(`{"name":"renamed","metadata":{"k":"v"}}`))
	req.SetPathValue("id", "env_1")
	w := httptest.NewRecorder()
	s.updateEnvironment(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var e cma.Environment
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Name != "renamed" {
		t.Errorf("name=%q", e.Name)
	}
	if e.Description != "d" {
		t.Errorf("description should be untouched, got %q", e.Description)
	}
	if e.Metadata["k"] != "v" {
		t.Errorf("metadata=%v", e.Metadata)
	}
}

func TestArchiveEnvironment_SetsArchivedAt(t *testing.T) {
	s := newServer(t, "")
	now := time.Now().UTC()
	_ = s.store.PutEnvironment(&cma.Environment{Type: "environment", ID: "env_a", Name: "a", Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now})
	req := httptest.NewRequest(http.MethodPost, "/v1/environments/env_a/archive", nil)
	req.SetPathValue("id", "env_a")
	w := httptest.NewRecorder()
	s.archiveEnvironment(w, req)
	var e cma.Environment
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.ArchivedAt == nil {
		t.Error("archived_at not set")
	}
}

func TestDeleteEnvironment_RemovesAndShape(t *testing.T) {
	s := newServer(t, "")
	now := time.Now().UTC()
	_ = s.store.PutEnvironment(&cma.Environment{Type: "environment", ID: "env_d", Name: "d", Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now})

	req := httptest.NewRequest(http.MethodDelete, "/v1/environments/env_d", nil)
	req.SetPathValue("id", "env_d")
	w := httptest.NewRecorder()
	s.deleteEnvironment(w, req)
	var d cma.DeletedResource
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.ID != "env_d" || d.Type != "environment_deleted" {
		t.Fatalf("delete response = %+v", d)
	}
	if _, ok := s.store.Environment("env_d"); ok {
		t.Error("environment still present after delete")
	}

	// Deleting again → 404.
	w2 := httptest.NewRecorder()
	s.deleteEnvironment(w2, req)
	if w2.Code != http.StatusNotFound {
		t.Errorf("second delete status=%d, want 404", w2.Code)
	}
}

func TestDeleteSession_EmitsEventAndRemoves(t *testing.T) {
	// gcAhsirAgentIfUnused will DeleteAgent once the session is gone; accept it.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	s := newServer(t, ts.URL)

	rec := &store.SessionRecord{
		Session:   &cma.Session{Type: "session", ID: "sesn_d", Status: cma.StatusIdle},
		AhsirName: "cma-x-v1", ContextID: "c",
	}
	_ = s.store.PutSession(rec)
	_, ch, cancel := rec.Subscribe()
	defer cancel()

	req := httptest.NewRequest(http.MethodDelete, "/v1/sessions/sesn_d", nil)
	req.SetPathValue("id", "sesn_d")
	w := httptest.NewRecorder()
	s.deleteSession(w, req)

	var d cma.DeletedResource
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.ID != "sesn_d" || d.Type != "session_deleted" {
		t.Fatalf("delete response = %+v", d)
	}
	// Live subscriber must have received session.deleted.
	select {
	case ev := <-ch:
		if ev.Type != cma.EvtSessionDeleted {
			t.Errorf("event = %q, want session.deleted", ev.Type)
		}
	default:
		t.Error("no session.deleted event delivered")
	}
	if _, ok := s.store.Session("sesn_d"); ok {
		t.Error("session still present after delete")
	}
}

func TestUpdateSession_Partial(t *testing.T) {
	s := newServer(t, "")
	rec := &store.SessionRecord{
		Session:   &cma.Session{Type: "session", ID: "sesn_u", Status: cma.StatusIdle, Title: "orig", Metadata: map[string]string{}},
		AhsirName: "a", ContextID: "c",
	}
	_ = s.store.PutSession(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sesn_u",
		strings.NewReader(`{"title":"renamed","metadata":{"a":"b"}}`))
	req.SetPathValue("id", "sesn_u")
	w := httptest.NewRecorder()
	s.updateSession(w, req)
	var out cma.Session
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Title != "renamed" || out.Metadata["a"] != "b" {
		t.Fatalf("update result = %+v", out)
	}
}
