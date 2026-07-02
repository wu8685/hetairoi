package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/cma"
	"github.com/wu8685/cma-service/internal/eventbus"
)

// a2aWithReply is an ahsir stand-in: admin register + a streaming turn that
// replies with the given text.
func a2aWithReply(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/admin/agents" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/a2a/") {
			var rpc struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&rpc)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			emit := func(ev map[string]any) {
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": ev})
				_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			}
			emit(map[string]any{"kind": "status-update", "taskId": "t", "contextId": "c",
				"status": map[string]any{"state": "working", "message": map[string]any{"parts": []map[string]any{{"kind": "text", "text": reply}}}}})
			emit(map[string]any{"kind": "task", "id": "t", "contextId": "c", "status": map[string]any{"state": "completed"}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// §12.11: SessionSummary is derived from the event log — first user.message is
// the seed, last agent.message is the current state. Also exercises the driver's
// CreateSession + SendUserMessage end to end against a stand-in ahsir.
func TestBusDriver_SummaryFromLog(t *testing.T) {
	srv := a2aWithReply(t, "agent-reply")
	defer srv.Close()
	s := newServer(t, srv.URL)
	s.ahsir = ahsir.New(srv.URL, "")

	now := time.Now().UTC()
	seedAgent(s, "agent_d", 1, false, now)
	_ = s.store.PutEnvironment(&cma.Environment{Type: "environment", ID: "env_d", Name: "e", Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now})

	d := s.BusDriver()
	sid, err := d.CreateSession(eventbus.AgentRef{ID: "agent_d"}, "env_d")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := d.SendUserMessage(sid, "hello world"); err != nil {
		t.Fatalf("SendUserMessage: %v", err)
	}

	rec, _ := s.store.Session(sid)
	waitForEvent(t, rec, func(evs []cma.Event) bool {
		for _, e := range evs {
			if e.Type == cma.EvtAgentMessage {
				return true
			}
		}
		return false
	}, 5*time.Second)
	time.Sleep(100 * time.Millisecond) // let the final status persist settle

	sum, err := d.SessionSummary(sid)
	if err != nil {
		t.Fatalf("SessionSummary: %v", err)
	}
	if sum.Seed != "hello world" {
		t.Errorf("seed = %q, want 'hello world' (first user.message)", sum.Seed)
	}
	if sum.Last != "agent-reply" {
		t.Errorf("last = %q, want 'agent-reply' (last agent.message)", sum.Last)
	}
	if sum.Archived {
		t.Error("summary should not be archived")
	}
}

// End to end: webhook → bus → driver → a real cma-service session → ahsir.
func TestEventBusWebhook_EndToEnd(t *testing.T) {
	srv := a2aWithReply(t, "handled")
	defer srv.Close()
	s := newServer(t, srv.URL)
	s.ahsir = ahsir.New(srv.URL, "")

	now := time.Now().UTC()
	seedAgent(s, "agent_e", 1, false, now)
	_ = s.store.PutEnvironment(&cma.Environment{Type: "environment", ID: "env_e", Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now})

	bus := eventbus.New(s.BusDriver(), t.TempDir(), 8)
	_ = bus.Register(eventbus.Subscription{
		Name:  "alerts",
		Match: func(e eventbus.Event) bool { return e.Type == "alert" },
		Policy: eventbus.Stateless{
			Agent: eventbus.AgentRef{ID: "agent_e"}, EnvID: "env_e",
			Prompt: func(e eventbus.Event) string { return "alert: " + e.Subject },
		},
	})
	s.SetEventBus(bus)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/eventbus/events",
		strings.NewReader(`{"id":"evt1","type":"alert","subject":"db-down"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []struct {
			SessionID string `json:"session_id"`
		} `json:"results"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 1 || resp.Results[0].SessionID == "" {
		t.Fatalf("expected one dispatched session, got %s", w.Body.String())
	}

	rec, ok := s.store.Session(resp.Results[0].SessionID)
	if !ok {
		t.Fatal("dispatched session not found in store")
	}
	waitForEvent(t, rec, func(evs []cma.Event) bool {
		var prompt, reply bool
		for _, e := range evs {
			if e.Type == cma.EvtUserMessage && textOf(e.Content) == "alert: db-down" {
				prompt = true
			}
			if e.Type == cma.EvtAgentMessage {
				reply = true
			}
		}
		return prompt && reply
	}, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
}

// The driver's CreateSession surfaces the classified errors (archived agent).
func TestBusDriver_CreateSessionArchivedAgent(t *testing.T) {
	s := newServer(t, "")
	now := time.Now().UTC()
	seedAgent(s, "agent_arch", 1, true, now) // archived
	_ = s.store.PutEnvironment(&cma.Environment{Type: "environment", ID: "env_x", Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now})
	_, err := s.BusDriver().CreateSession(eventbus.AgentRef{ID: "agent_arch"}, "env_x")
	if err == nil {
		t.Fatal("expected error creating a session on an archived agent")
	}
}
