package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/cma"
	"github.com/wu8685/cma-service/internal/store"
)

// waitForEvent polls the session log (lock-safe) until pred is satisfied.
func waitForEvent(t *testing.T, rec *store.SessionRecord, pred func([]cma.Event) bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred(rec.Snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v; log=%+v", d, rec.Snapshot())
}

// TestInterruptCancelsInFlightTurn drives a turn whose A2A stream blocks after
// announcing its taskId, sends user.interrupt through the real handler, and
// asserts the turn is cancelled (tasks/cancel with the right id), partial text
// is surfaced, and the session settles back to idle.
func TestInterruptCancelsInFlightTurn(t *testing.T) {
	cancelCh := make(chan struct{})
	var mu sync.Mutex
	var canceledTask string

	emit := func(w http.ResponseWriter, fl http.Flusher, rpcID string, ev map[string]any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpcID, "result": ev})
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		fl.Flush()
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     string          `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		switch rpc.Method {
		case "tasks/cancel":
			var p struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rpc.Params, &p)
			mu.Lock()
			canceledTask = p.ID
			mu.Unlock()
			close(cancelCh)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": map[string]any{}})
		case "message/stream":
			fl := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			emit(w, fl, rpc.ID, map[string]any{"kind": "status-update", "taskId": "task_int", "contextId": "c",
				"status": map[string]any{"state": "working"}})
			emit(w, fl, rpc.ID, map[string]any{"kind": "status-update", "taskId": "task_int", "contextId": "c",
				"status": map[string]any{"state": "working",
					"message": map[string]any{"parts": []map[string]any{{"kind": "text", "text": "partial"}}}}})
			<-cancelCh // block until the interrupt arrives
			emit(w, fl, rpc.ID, map[string]any{"kind": "task", "id": "task_int", "contextId": "c",
				"status": map[string]any{"state": "canceled"}})
		}
	}))
	defer srv.Close()

	st, err := store.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &store.SessionRecord{
		Session:   &cma.Session{Type: "session", ID: "sesn_i", Status: cma.StatusIdle},
		AhsirName: "a", ContextID: "c",
	}
	_ = st.PutSession(rec)
	s := &Server{store: st, ahsir: ahsir.New(srv.URL, ""), registered: map[string]bool{}}

	s.runTurn(rec, "hi")

	// Wait until the turn has published its cancelable taskId.
	deadline := time.Now().Add(3 * time.Second)
	for rec.InFlightTask() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rec.InFlightTask() != "task_int" {
		t.Fatalf("in-flight task = %q, want task_int", rec.InFlightTask())
	}

	// Send user.interrupt through the real handler.
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sesn_i/events",
		strings.NewReader(`{"events":[{"type":"user.interrupt"}]}`))
	req.SetPathValue("id", "sesn_i")
	w := httptest.NewRecorder()
	s.sendEvents(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("interrupt status = %d", w.Code)
	}

	// The session must settle to idle, with the partial text surfaced.
	waitForEvent(t, rec, func(evs []cma.Event) bool {
		var idle, partial bool
		for _, ev := range evs {
			if ev.Type == cma.EvtSessionStatusIdle {
				idle = true
			}
			if ev.Type == cma.EvtAgentMessage {
				for _, b := range ev.Content {
					if b.Text == "partial" {
						partial = true
					}
				}
			}
		}
		return idle && partial
	}, 3*time.Second)

	mu.Lock()
	ct := canceledTask
	mu.Unlock()
	if ct != "task_int" {
		t.Errorf("canceled task = %q, want task_int", ct)
	}
	if rec.InFlightTask() != "" {
		t.Errorf("in-flight task should be cleared after turn, got %q", rec.InFlightTask())
	}
}
