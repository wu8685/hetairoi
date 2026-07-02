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
	"github.com/wu8685/cma-service/internal/store"
)

func seedAgent(s *Server, id string, version int64, archived bool, created time.Time) {
	a := &cma.Agent{Type: "agent", ID: id, Version: version, Name: id, CreatedAt: created, UpdatedAt: created}
	if archived {
		a.ArchivedAt = &created
	}
	normalizeAgent(a)
	_ = s.store.PutAgentVersion(a)
}

func listAgentsResp(t *testing.T, s *Server, query string) cma.List[*cma.Agent] {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/agents?"+query, nil)
	w := httptest.NewRecorder()
	s.listAgents(w, req)
	var out cma.List[*cma.Agent]
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestList_ExcludesArchivedByDefault(t *testing.T) {
	s := newServer(t, "")
	base := time.Now().UTC()
	seedAgent(s, "agent_a", 1, false, base)
	seedAgent(s, "agent_b", 1, true, base.Add(time.Second))
	seedAgent(s, "agent_c", 1, false, base.Add(2*time.Second))

	def := listAgentsResp(t, s, "")
	if len(def.Data) != 2 {
		t.Fatalf("default list = %d, want 2 (archived excluded)", len(def.Data))
	}
	for _, a := range def.Data {
		if a.ArchivedAt != nil {
			t.Errorf("archived agent %s leaked into default list", a.ID)
		}
	}
	// Deterministic created_at order.
	if def.Data[0].ID != "agent_a" || def.Data[1].ID != "agent_c" {
		t.Errorf("order = %s,%s want agent_a,agent_c", def.Data[0].ID, def.Data[1].ID)
	}

	all := listAgentsResp(t, s, "include_archived=true")
	if len(all.Data) != 3 {
		t.Fatalf("include_archived list = %d, want 3", len(all.Data))
	}
}

func TestList_Pagination(t *testing.T) {
	s := newServer(t, "")
	base := time.Now().UTC()
	for i := 0; i < 3; i++ {
		seedAgent(s, fmt.Sprintf("agent_%d", i), 1, false, base.Add(time.Duration(i)*time.Second))
	}
	p1 := listAgentsResp(t, s, "limit=2")
	if len(p1.Data) != 2 || p1.NextPage == nil {
		t.Fatalf("page1 len=%d next=%v", len(p1.Data), p1.NextPage)
	}
	p2 := listAgentsResp(t, s, "limit=2&page="+*p1.NextPage)
	if len(p2.Data) != 1 || p2.NextPage != nil {
		t.Fatalf("page2 len=%d next=%v", len(p2.Data), p2.NextPage)
	}
}

func TestCreateSession_RejectsArchived(t *testing.T) {
	s := newServer(t, "")
	now := time.Now().UTC()
	seedAgent(s, "agent_arch", 1, true, now)
	_ = s.store.PutEnvironment(&cma.Environment{Type: "environment", ID: "env_ok", Name: "e", Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now})

	body := `{"agent":{"id":"agent_arch"},"environment_id":"env_ok"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.createSession(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("archived agent: status=%d body=%s, want 400", w.Code, w.Body.String())
	}

	// Archived environment is rejected too.
	seedAgent(s, "agent_ok", 1, false, now)
	_ = s.store.PutEnvironment(&cma.Environment{Type: "environment", ID: "env_arch", Name: "e", Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now, ArchivedAt: &now})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/sessions",
		strings.NewReader(`{"agent":{"id":"agent_ok"},"environment_id":"env_arch"}`))
	w2 := httptest.NewRecorder()
	s.createSession(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("archived env: status=%d, want 400", w2.Code)
	}
}

func TestArchiveAgent_AllVersions(t *testing.T) {
	s := newServer(t, "")
	now := time.Now().UTC()
	seedAgent(s, "agent_v", 1, false, now)
	seedAgent(s, "agent_v", 2, false, now)

	req := httptest.NewRequest(http.MethodPost, "/v1/agents/agent_v/archive", nil)
	req.SetPathValue("id", "agent_v")
	w := httptest.NewRecorder()
	s.archiveAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	for _, v := range []int64{1, 2} {
		a, _ := s.store.Agent("agent_v", v)
		if a.ArchivedAt == nil {
			t.Errorf("version %d not archived", v)
		}
	}
}

// TestExecuteTurn_EmitsRescheduled drives a turn whose first connect 502s
// (agent briefly unreachable / being rescheduled) and asserts a
// session.status_rescheduled event is emitted before the reply.
func TestExecuteTurn_EmitsRescheduled(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, `{"error":"proxy: connection refused"}`, http.StatusBadGateway)
			return
		}
		var rpc struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range []map[string]any{
			{"kind": "status-update", "taskId": "t1", "contextId": "c", "status": map[string]any{"state": "working", "message": map[string]any{"parts": []map[string]any{{"kind": "text", "text": "hi"}}}}},
			{"kind": "task", "id": "t1", "contextId": "c", "status": map[string]any{"state": "completed"}},
		} {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": ev})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
	}))
	defer srv.Close()

	s := newServer(t, srv.URL)
	s.ahsir = ahsir.New(srv.URL, "")
	rec := &store.SessionRecord{
		Session:   &cma.Session{Type: "session", ID: "sesn_r", Status: cma.StatusIdle},
		AhsirName: "a", ContextID: "c",
	}
	_ = s.store.PutSession(rec)

	s.runTurn(rec, "hello")
	waitForEvent(t, rec, func(evs []cma.Event) bool {
		var resched, idle bool
		for _, ev := range evs {
			if ev.Type == cma.EvtSessionStatusRescheduled {
				resched = true
			}
			if ev.Type == cma.EvtSessionStatusIdle {
				idle = true
			}
		}
		return resched && idle
	}, 5*time.Second)
	// Let the final status persist settle before TempDir cleanup.
	time.Sleep(100 * time.Millisecond)
}
