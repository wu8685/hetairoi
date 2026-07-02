package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/cma"
	"github.com/wu8685/cma-service/internal/store"
)

// dataPartA2A returns an httptest A2A server whose message/stream emits the given
// observability DataParts (plus a tiny text reply and terminal task).
func dataPartA2A(t *testing.T, dataParts []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		emit(map[string]any{"kind": "status-update", "taskId": "t1", "contextId": "c", "status": map[string]any{"state": "working"}})
		for _, d := range dataParts {
			emit(map[string]any{"kind": "status-update", "taskId": "t1", "contextId": "c",
				"status": map[string]any{"state": "working",
					"message": map[string]any{"parts": []map[string]any{{"kind": "data", "data": d}}}}})
		}
		emit(map[string]any{"kind": "status-update", "taskId": "t1", "contextId": "c",
			"status": map[string]any{"state": "working", "message": map[string]any{"parts": []map[string]any{{"kind": "text", "text": "ok"}}}}})
		emit(map[string]any{"kind": "task", "id": "t1", "contextId": "c", "status": map[string]any{"state": "completed"}})
	}))
}

func runTurnAndCollect(t *testing.T, dataParts []map[string]any) []cma.Event {
	t.Helper()
	srv := dataPartA2A(t, dataParts)
	t.Cleanup(srv.Close)
	s := newServer(t, srv.URL)
	s.ahsir = ahsir.New(srv.URL, "")
	rec := &store.SessionRecord{
		Session:   &cma.Session{Type: "session", ID: "sesn_o", Status: cma.StatusIdle},
		AhsirName: "a", ContextID: "c",
	}
	_ = s.store.PutSession(rec)
	s.runTurn(rec, "go")
	waitForEvent(t, rec, func(evs []cma.Event) bool {
		for _, e := range evs {
			if e.Type == cma.EvtSessionStatusIdle {
				return true
			}
		}
		return false
	}, 5*time.Second)
	// Let the turn goroutine's final status persist settle before the test's
	// TempDir cleanup runs, so a state-file write can't race the dir removal.
	time.Sleep(100 * time.Millisecond)
	return rec.Snapshot()
}

func TestObservability_ToolUseAndSpan(t *testing.T) {
	evs := runTurnAndCollect(t, []map[string]any{
		{"ev": "span_start"},
		{"ev": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"path": "/x"}},
		{"ev": "span_end", "usage": map[string]any{"input_tokens": 7, "output_tokens": 3}},
	})

	var tool, spanStart, spanEnd *cma.Event
	for i := range evs {
		switch evs[i].Type {
		case cma.EvtAgentToolUse:
			tool = &evs[i]
		case cma.EvtSpanModelRequestStart:
			spanStart = &evs[i]
		case cma.EvtSpanModelRequestEnd:
			spanEnd = &evs[i]
		}
	}
	if tool == nil || spanStart == nil || spanEnd == nil {
		t.Fatalf("missing events: tool=%v start=%v end=%v\nlog=%+v", tool != nil, spanStart != nil, spanEnd != nil, evs)
	}
	if tool.ID != "toolu_1" || tool.Name != "Read" || string(tool.Input) == "" {
		t.Errorf("tool_use = %+v", tool)
	}
	// span_end links back to span_start and carries usage.
	if spanEnd.ModelRequestStartID != spanStart.ID {
		t.Errorf("span_end.model_request_start_id=%q, want %q", spanEnd.ModelRequestStartID, spanStart.ID)
	}
	if spanEnd.ModelUsage == nil || spanEnd.ModelUsage.InputTokens != 7 || spanEnd.ModelUsage.OutputTokens != 3 {
		t.Errorf("span_end.model_usage = %+v", spanEnd.ModelUsage)
	}
}

func TestObservability_MCPToolClassification(t *testing.T) {
	evs := runTurnAndCollect(t, []map[string]any{
		{"ev": "tool_use", "id": "toolu_2", "name": "mcp__github__create_issue", "input": map[string]any{"title": "x"}},
		{"ev": "tool_result", "tool_use_id": "toolu_2", "content": "ok", "is_error": false},
	})
	var mcpUse, mcpResult *cma.Event
	for i := range evs {
		switch evs[i].Type {
		case cma.EvtAgentMCPToolUse:
			mcpUse = &evs[i]
		case cma.EvtAgentMCPToolResult:
			mcpResult = &evs[i]
		case cma.EvtAgentToolUse, cma.EvtAgentToolResult:
			t.Errorf("MCP event wrongly classified as non-MCP: %+v", evs[i])
		}
	}
	if mcpUse == nil || mcpResult == nil {
		t.Fatalf("missing mcp events: use=%v result=%v; log=%+v", mcpUse != nil, mcpResult != nil, evs)
	}
	if mcpUse.MCPServerName != "github" || mcpUse.Name != "create_issue" {
		t.Errorf("mcp_tool_use = server=%q name=%q", mcpUse.MCPServerName, mcpUse.Name)
	}
	// The result inherits MCP classification via the tool_use id and links back.
	if mcpResult.MCPToolUseID != "toolu_2" {
		t.Errorf("mcp_tool_result.mcp_tool_use_id = %q, want toolu_2", mcpResult.MCPToolUseID)
	}
}

func TestObservability_ToolResultAndThinking(t *testing.T) {
	evs := runTurnAndCollect(t, []map[string]any{
		{"ev": "thinking"},
		{"ev": "tool_use", "id": "toolu_9", "name": "Read", "input": map[string]any{"path": "/x"}},
		{"ev": "tool_result", "tool_use_id": "toolu_9", "content": "file contents", "is_error": false},
	})
	var thinking, result *cma.Event
	for i := range evs {
		switch evs[i].Type {
		case cma.EvtAgentThinking:
			thinking = &evs[i]
		case cma.EvtAgentToolResult:
			result = &evs[i]
		}
	}
	if thinking == nil {
		t.Errorf("no agent.thinking emitted; log=%+v", evs)
	}
	if result == nil {
		t.Fatalf("no agent.tool_result emitted; log=%+v", evs)
	}
	if result.ToolUseID != "toolu_9" {
		t.Errorf("tool_result.tool_use_id = %q, want toolu_9", result.ToolUseID)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "file contents" {
		t.Errorf("tool_result content = %+v", result.Content)
	}
}
