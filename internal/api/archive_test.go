package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wu8685/hetairoi/internal/ahsir"
	"github.com/wu8685/hetairoi/internal/cma"
	"github.com/wu8685/hetairoi/internal/store"
)

// TestArchiveSessionRefcountedGC verifies the shared ahsir agent is reclaimed
// only when the last live session pinning it is archived — never while another
// session still references it.
func TestArchiveSessionRefcountedGC(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	st, err := store.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	const shared = "cma-x-v1"
	for _, id := range []string{"sesn_1", "sesn_2"} {
		_ = st.PutSession(&store.SessionRecord{
			Session:   &cma.Session{Type: "session", ID: id, Status: cma.StatusIdle},
			AhsirName: shared, ContextID: "ctx_" + id,
		})
	}

	s := &Server{
		store:      st,
		ahsir:      ahsir.New(ts.URL, ""),
		registered: map[string]bool{shared: true},
	}

	archive := func(id string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/archive", nil)
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		s.archiveSession(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("archive %s: status %d", id, w.Code)
		}
	}

	// First archive: one live session remains → no GC.
	archive("sesn_1")
	mu.Lock()
	n1 := len(deleted)
	mu.Unlock()
	if n1 != 0 {
		t.Fatalf("agent deleted too early: %v", deleted)
	}

	// Second archive: refcount hits zero → GC fires once, and the registered
	// flag is cleared so a future session re-registers.
	archive("sesn_2")
	mu.Lock()
	got := append([]string(nil), deleted...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "/admin/agents/"+shared {
		t.Fatalf("expected single delete of %s, got %v", shared, got)
	}
	s.regMu.Lock()
	stillRegistered := s.registered[shared]
	s.regMu.Unlock()
	if stillRegistered {
		t.Error("registered flag should be cleared after GC")
	}
}
