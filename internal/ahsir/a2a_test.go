package ahsir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer returns an httptest server whose POST /a2a/{name} emits the given
// pre-rendered SSE frames for message/stream, and records tasks/cancel calls.
func sseServer(t *testing.T, frames []map[string]any, onCancel func(id string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     string          `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		if rpc.Method == "tasks/cancel" {
			var p struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rpc.Params, &p)
			if onCancel != nil {
				onCancel(p.ID)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": map[string]any{}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range frames {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": ev})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
	}))
}

func su(taskID, text string) map[string]any {
	status := map[string]any{"state": "working"}
	if text != "" {
		status["message"] = map[string]any{"parts": []map[string]any{{"kind": "text", "text": text}}}
	}
	return map[string]any{"kind": "status-update", "taskId": taskID, "contextId": "c", "status": status}
}

func taskEv(taskID, state string) map[string]any {
	return map[string]any{"kind": "task", "id": taskID, "contextId": "c", "status": map[string]any{"state": state}}
}

func TestChatStream_AggregatesDeltas(t *testing.T) {
	frames := []map[string]any{
		su("task_1", ""),       // working, taskId only
		su("task_1", "Hello "), // delta
		su("task_1", "world"),  // delta
		taskEv("task_1", "completed"),
	}
	ts := sseServer(t, frames, nil)
	defer ts.Close()
	c := New(ts.URL, "")

	var gotTaskID string
	reply, err := c.ChatStream(context.Background(), "agent", "c", "hi", func(id string) { gotTaskID = id }, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if reply != "Hello world" {
		t.Errorf("reply = %q, want %q", reply, "Hello world")
	}
	if gotTaskID != "task_1" {
		t.Errorf("taskID = %q, want task_1", gotTaskID)
	}
}

func TestChatStream_TerminalTaskTextWhenNoDeltas(t *testing.T) {
	// No delta status-updates; the terminal task carries the full message.
	frames := []map[string]any{
		su("task_2", ""),
		{"kind": "task", "id": "task_2", "contextId": "c",
			"status": map[string]any{"state": "completed",
				"message": map[string]any{"parts": []map[string]any{{"kind": "text", "text": "full reply"}}}}},
	}
	ts := sseServer(t, frames, nil)
	defer ts.Close()
	reply, err := New(ts.URL, "").ChatStream(context.Background(), "agent", "c", "hi", nil, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if reply != "full reply" {
		t.Errorf("reply = %q, want 'full reply'", reply)
	}
}

func TestChatStream_Canceled(t *testing.T) {
	frames := []map[string]any{
		su("task_3", "partial"),
		taskEv("task_3", "canceled"),
	}
	ts := sseServer(t, frames, nil)
	defer ts.Close()
	reply, err := New(ts.URL, "").ChatStream(context.Background(), "agent", "c", "hi", nil, nil, nil)
	if !errors.Is(err, ErrTurnCanceled) {
		t.Fatalf("err = %v, want ErrTurnCanceled", err)
	}
	if reply != "partial" {
		t.Errorf("partial reply = %q, want 'partial'", reply)
	}
}

func TestChatStream_RPCError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":"1","error":{"code":-32000,"message":"boom"}}`+"\n\n")
	}))
	defer ts.Close()
	_, err := New(ts.URL, "").ChatStream(context.Background(), "agent", "c", "hi", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want rpc error 'boom'", err)
	}
}

// TestChatStream_RetryCallsReschedule verifies the connect-retry path: a 502
// (gateway can't reach a still-binding/rescheduling agent) is retried, and
// onReschedule fires exactly once when the first retry happens.
func TestChatStream_RetryCallsReschedule(t *testing.T) {
	var attempts int
	frames := []map[string]any{su("task_r", "ok"), taskEv("task_r", "completed")}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		for _, ev := range frames {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": ev})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
	}))
	defer ts.Close()

	var rescheduled int
	reply, err := New(ts.URL, "").ChatStream(context.Background(), "agent", "c", "hi", nil,
		func() { rescheduled++ }, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if reply != "ok" {
		t.Errorf("reply = %q, want ok", reply)
	}
	if rescheduled != 1 {
		t.Errorf("onReschedule fired %d times, want 1", rescheduled)
	}
	if attempts < 2 {
		t.Errorf("expected a retry, got %d attempts", attempts)
	}
}

func dataUp(taskID string, data map[string]any) map[string]any {
	return map[string]any{"kind": "status-update", "taskId": taskID, "contextId": "c",
		"status": map[string]any{"state": "working",
			"message": map[string]any{"parts": []map[string]any{{"kind": "data", "data": data}}}}}
}

// TestChatStream_ParsesDataParts verifies structured DataParts are decoded into
// StreamEvents and delivered via onEvent, while text deltas still aggregate.
func TestChatStream_ParsesDataParts(t *testing.T) {
	frames := []map[string]any{
		su("task_d", ""),
		dataUp("task_d", map[string]any{"ev": "span_start"}),
		dataUp("task_d", map[string]any{"ev": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"path": "/x"}}),
		su("task_d", "hello"),
		dataUp("task_d", map[string]any{"ev": "span_end", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}),
		taskEv("task_d", "completed"),
	}
	ts := sseServer(t, frames, nil)
	defer ts.Close()

	var got []StreamEvent
	reply, err := New(ts.URL, "").ChatStream(context.Background(), "agent", "c", "hi", nil, nil,
		func(se StreamEvent) { got = append(got, se) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if reply != "hello" {
		t.Errorf("reply = %q, want hello (text still aggregates)", reply)
	}
	if len(got) != 3 {
		t.Fatalf("got %d stream events, want 3: %+v", len(got), got)
	}
	if got[0].Kind != "span_start" {
		t.Errorf("ev0 = %+v", got[0])
	}
	if got[1].Kind != "tool_use" || got[1].ID != "toolu_1" || got[1].Name != "Read" || string(got[1].Input) == "" {
		t.Errorf("ev1 = %+v (input=%s)", got[1], got[1].Input)
	}
	if got[2].Kind != "span_end" || got[2].Usage == nil || got[2].Usage.InputTokens != 10 || got[2].Usage.OutputTokens != 5 {
		t.Errorf("ev2 = %+v usage=%+v", got[2], got[2].Usage)
	}
}

func TestCancelTask_SendsTaskID(t *testing.T) {
	var canceled string
	ts := sseServer(t, nil, func(id string) { canceled = id })
	defer ts.Close()
	if err := New(ts.URL, "").CancelTask(context.Background(), "agent", "task_9"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if canceled != "task_9" {
		t.Errorf("canceled = %q, want task_9", canceled)
	}
}
