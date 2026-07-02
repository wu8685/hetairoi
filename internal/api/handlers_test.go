package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/wu8685/cma-service/internal/cma"
	"github.com/wu8685/cma-service/internal/store"
)

func TestNormalizeAgent(t *testing.T) {
	a := &cma.Agent{ID: "agent_1"}
	normalizeAgent(a)
	if a.Tools == nil || a.Skills == nil || a.MCPServers == nil || a.Metadata == nil {
		t.Fatalf("nil required field after normalize: %+v", a)
	}
	if len(a.Tools) != 0 || len(a.Skills) != 0 || len(a.MCPServers) != 0 || len(a.Metadata) != 0 {
		t.Errorf("expected empty (not nil) collections: %+v", a)
	}
}

func seedSession(t *testing.T, n int) (*Server, string) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &store.SessionRecord{
		Session:   &cma.Session{Type: "session", ID: "sesn_1", Status: cma.StatusIdle},
		AhsirName: "cma-x-v1", ContextID: "ctx_1",
	}
	if err := st.PutSession(rec); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		_ = st.AppendEvent(rec, cma.Event{ID: "ev_" + string(rune('a'+i)), Type: "agent.message"})
	}
	return &Server{store: st, registered: map[string]bool{}}, "sesn_1"
}

func listEvents(t *testing.T, s *Server, id, query string) cma.List[cma.Event] {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+id+"/events?"+query, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.listEvents(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out cma.List[cma.Event]
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	return out
}

func ids(evs []cma.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func TestListEvents_NoLimitReturnsAll(t *testing.T) {
	s, id := seedSession(t, 5)
	out := listEvents(t, s, id, "")
	if len(out.Data) != 5 {
		t.Fatalf("len = %d, want 5", len(out.Data))
	}
	if out.NextPage != nil {
		t.Errorf("next_page should be nil without limit, got %v", *out.NextPage)
	}
}

func TestListEvents_LimitAndCursor(t *testing.T) {
	s, id := seedSession(t, 5) // ev_a..ev_e

	p1 := listEvents(t, s, id, "limit=2")
	if got := ids(p1.Data); len(got) != 2 || got[0] != "ev_a" || got[1] != "ev_b" {
		t.Fatalf("page1 = %v", got)
	}
	if p1.NextPage == nil || *p1.NextPage != "ev_b" {
		t.Fatalf("page1 next_page = %v, want ev_b", p1.NextPage)
	}

	p2 := listEvents(t, s, id, "limit=2&page=ev_b")
	if got := ids(p2.Data); len(got) != 2 || got[0] != "ev_c" || got[1] != "ev_d" {
		t.Fatalf("page2 = %v", got)
	}

	p3 := listEvents(t, s, id, "limit=2&page=ev_d")
	if got := ids(p3.Data); len(got) != 1 || got[0] != "ev_e" {
		t.Fatalf("page3 = %v", got)
	}
	if p3.NextPage != nil {
		t.Errorf("final page next_page = %v, want nil", *p3.NextPage)
	}
}

func TestListEvents_DescOrder(t *testing.T) {
	s, id := seedSession(t, 3) // ev_a, ev_b, ev_c
	out := listEvents(t, s, id, "order=desc")
	if got := ids(out.Data); len(got) != 3 || got[0] != "ev_c" || got[2] != "ev_a" {
		t.Fatalf("desc = %v", got)
	}
}

func TestListEvents_UnknownCursorEmptyPage(t *testing.T) {
	s, id := seedSession(t, 3)
	out := listEvents(t, s, id, "page=ev_zzz")
	if len(out.Data) != 0 {
		t.Errorf("unknown cursor should yield empty page, got %v", ids(out.Data))
	}
}
