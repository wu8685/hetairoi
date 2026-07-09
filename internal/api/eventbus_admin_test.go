package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wu8685/hetairoi/internal/config"
	"github.com/wu8685/hetairoi/internal/eventbus"
)

// fakeDriver is a minimal eventbus.SessionDriver for the HTTP-surface tests.
type fakeDriver struct {
	mu     sync.Mutex
	nextID int
	sent   int
}

func (f *fakeDriver) CreateSession(agent eventbus.AgentRef, envID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return fmt.Sprintf("sesn_%d", f.nextID), nil
}
func (f *fakeDriver) SendUserMessage(sessionID, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	return nil
}
func (f *fakeDriver) RunForReply(agent eventbus.AgentRef, envID, prompt string) (string, error) {
	return "", nil
}
func (f *fakeDriver) SessionSummary(sessionID string) (eventbus.SessionSummary, error) {
	return eventbus.SessionSummary{SessionID: sessionID}, nil
}

// serverWithSeededHandler wires a Server over a real registry, registers a keyed
// handler, and dispatches one event so a retryable payload is stored.
func serverWithSeededHandler(t *testing.T) (*Server, *fakeDriver) {
	t.Helper()
	dir := t.TempDir()
	drv := &fakeDriver{}
	bus := eventbus.New(drv, dir, 8)
	reg, err := eventbus.NewRegistry(context.Background(), bus, dir)
	if err != nil {
		t.Fatal(err)
	}
	spec := eventbus.HandlerSpec{
		Name:  "ahsir-build",
		Match: eventbus.MatchSpec{Type: "issue"},
		Policy: eventbus.PolicySpec{
			Kind:           "keyed",
			AgentID:        "agent_x",
			KeyTemplate:    "issue-{{.subject}}",
			PromptTemplate: "build {{.subject}}",
		},
	}
	if err := reg.AddHandler(spec); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"n": 6})
	bus.Dispatch(eventbus.Event{ID: "gh-issue-6-abc", Type: "issue", Subject: "6", Payload: payload})

	s := New(config.Config{})
	s.SetEventRegistry(reg)
	return s, drv
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRetryEndpoint_ReplaysWithoutUpstreamChange(t *testing.T) {
	s, drv := serverWithSeededHandler(t)
	h := s.Handler()

	rec := post(t, h, "/v1/eventbus/handlers/ahsir-build/retry", `{"key":"issue-6"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []struct {
			Subscription string `json:"subscription"`
			SessionID    string `json:"session_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].SessionID == "" {
		t.Fatalf("expected one result with a session id, got %+v", resp.Results)
	}
	if drv.sent != 2 { // 1 from the seed dispatch + 1 from the retry
		t.Fatalf("driver sent %d, want 2", drv.sent)
	}
}

func TestRetryEndpoint_FreshSession(t *testing.T) {
	s, _ := serverWithSeededHandler(t)
	h := s.Handler()
	rec := post(t, h, "/v1/eventbus/handlers/ahsir-build/retry", `{"key":"issue-6","fresh_session":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestRetryEndpoint_UnknownHandler404(t *testing.T) {
	s, _ := serverWithSeededHandler(t)
	rec := post(t, s.Handler(), "/v1/eventbus/handlers/nope/retry", `{"key":"issue-6"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRetryEndpoint_UnknownKey404(t *testing.T) {
	s, _ := serverWithSeededHandler(t)
	rec := post(t, s.Handler(), "/v1/eventbus/handlers/ahsir-build/retry", `{"key":"issue-999"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestRetryEndpoint_NoSelector400(t *testing.T) {
	s, _ := serverWithSeededHandler(t)
	rec := post(t, s.Handler(), "/v1/eventbus/handlers/ahsir-build/retry", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestRetryEndpoint_BadBody400(t *testing.T) {
	s, _ := serverWithSeededHandler(t)
	rec := post(t, s.Handler(), "/v1/eventbus/handlers/ahsir-build/retry", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
