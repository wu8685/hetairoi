// Command fakeahsir is a minimal stand-in for the ahsir scheduler gateway, used
// by the e2e suite so the official Anthropic SDK can drive hetairoi end-to-end
// without a real ahsir fleet or a live LLM. It implements exactly the endpoints
// hetairoi depends on:
//
//	POST   /admin/agents                      register an agent (inline card) -> 201
//	DELETE /admin/agents/{name}               stop an agent -> 204
//	POST   /a2a/{name}                         A2A JSON-RPC: message/stream (SSE) + tasks/cancel
//	POST   /agents/{name}/chat                one turn (sync) -> {"response": "..."}
//	GET    /agents/{name}/history/{contextId} transcript -> []
//
// The streamed reply is deterministic (echoes the prompt across a couple of
// text deltas) so assertions are stable. A prompt containing "__SLOW__" makes
// the stream stall cancelably between deltas so the interrupt path can be
// exercised.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// cancels maps an in-flight A2A taskId to a channel closed when tasks/cancel
// fires for it. Lets message/stream end in the canceled state.
var (
	cancelMu sync.Mutex
	cancels  = map[string]chan struct{}{}
)

func main() {
	addr := os.Getenv("FAKEAHSIR_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:9801"
	}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /admin/agents", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["name"].(string)
		log.Printf("register agent %q (card present=%v)", name, body["card"] != nil)
		writeJSON(w, http.StatusCreated, map[string]any{"name": name, "port": 0})
	})

	mux.HandleFunc("DELETE /admin/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("delete agent %q", r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /a2a/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var rpc struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     string          `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		switch rpc.Method {
		case "message/stream":
			streamTurn(w, r, name, rpc.Params, rpc.ID)
		case "tasks/cancel":
			var p struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rpc.Params, &p)
			cancelMu.Lock()
			ch := cancels[p.ID]
			delete(cancels, p.ID)
			cancelMu.Unlock()
			if ch != nil {
				close(ch)
			}
			log.Printf("cancel task %q on %q", p.ID, name)
			writeJSON(w, http.StatusOK, rpcResult(rpc.ID, taskEvent(p.ID, "", "canceled")))
		default:
			writeJSON(w, http.StatusOK, rpcResult(rpc.ID, map[string]any{}))
		}
	})

	mux.HandleFunc("POST /agents/{name}/chat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Message   string `json:"message"`
			ContextID string `json:"contextId"`
			Speaker   string `json:"speaker"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := "Echo from " + r.PathValue("name") + ": " + strings.TrimSpace(req.Message)
		log.Printf("chat %q ctx=%s msg=%q", r.PathValue("name"), req.ContextID, req.Message)
		writeJSON(w, http.StatusOK, map[string]any{"response": reply})
	})

	mux.HandleFunc("GET /agents/{name}/history/{contextId}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("fakeahsir listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// streamTurn serves one A2A message/stream turn as SSE: an initial working
// status-update (so the client learns the taskId), then the echoed reply split
// across two text-delta status-updates, then a terminal completed task. With
// "__SLOW__" in the prompt it stalls cancelably between deltas.
func streamTurn(w http.ResponseWriter, r *http.Request, name string, params json.RawMessage, rpcID string) {
	var p struct {
		Message struct {
			ContextID string `json:"contextId"`
			Parts     []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"message"`
	}
	_ = json.Unmarshal(params, &p)
	prompt := ""
	for _, part := range p.Message.Parts {
		prompt += part.Text
	}
	ctxID := p.Message.ContextID
	taskID := "task_" + randHex()
	slow := strings.Contains(prompt, "__SLOW__")

	cancelCh := make(chan struct{})
	cancelMu.Lock()
	cancels[taskID] = cancelCh
	cancelMu.Unlock()
	defer func() {
		cancelMu.Lock()
		delete(cancels, taskID)
		cancelMu.Unlock()
	}()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	emit := func(ev any) {
		_, _ = w.Write([]byte("data: "))
		b, _ := json.Marshal(rpcResult(rpcID, ev))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Initial working frame carries the taskId (no text yet) so the client can
	// learn the cancelable task before any interrupt.
	emit(statusUpdate(taskID, ctxID, ""))

	// Observability DataParts (C1): a model-request span around the turn and a
	// tool invocation, so the e2e exercises the structured-event path.
	emit(dataUpdate(taskID, ctxID, map[string]any{"ev": "span_start"}))
	// Unique per turn: the CMA agent.tool_use event id IS this tool-use id, and
	// event ids must be unique across the log for cursor pagination.
	emit(dataUpdate(taskID, ctxID, map[string]any{"ev": "tool_use", "id": "toolu_" + taskID, "name": "Read", "input": map[string]any{"path": "/etc/hostname"}}))
	emit(dataUpdate(taskID, ctxID, map[string]any{"ev": "tool_result", "tool_use_id": "toolu_" + taskID, "content": "myhost", "is_error": false}))

	full := "Echo from " + name + ": " + strings.TrimSpace(prompt)

	// In slow mode, stall (cancelably) after announcing the taskId so the
	// interrupt path has a deterministic window. A long fallback prevents a
	// hung test if no cancel ever arrives.
	if slow {
		select {
		case <-cancelCh:
			emit(statusUpdate(taskID, ctxID, "partial before cancel"))
			emit(taskEvent(taskID, ctxID, "canceled"))
			return
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
		}
	}

	emit(statusUpdate(taskID, ctxID, full[:len(full)/2]))
	emit(statusUpdate(taskID, ctxID, full[len(full)/2:]))
	emit(dataUpdate(taskID, ctxID, map[string]any{"ev": "span_end", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}))
	emit(taskEvent(taskID, ctxID, "completed"))
}

func rpcResult(id string, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// statusUpdate builds an A2A status-update event; text "" means a working
// announcement with no delta.
func statusUpdate(taskID, ctxID, text string) map[string]any {
	status := map[string]any{"state": "working"}
	if text != "" {
		status["message"] = map[string]any{
			"role":  "agent",
			"parts": []map[string]any{{"kind": "text", "text": text}},
		}
	}
	return map[string]any{
		"kind":      "status-update",
		"taskId":    taskID,
		"contextId": ctxID,
		"final":     false,
		"status":    status,
	}
}

// dataUpdate builds a status-update whose message carries a single DataPart —
// the structured observability channel (tool_use, span_start, span_end).
func dataUpdate(taskID, ctxID string, data map[string]any) map[string]any {
	return map[string]any{
		"kind":      "status-update",
		"taskId":    taskID,
		"contextId": ctxID,
		"final":     false,
		"status": map[string]any{
			"state":   "working",
			"message": map[string]any{"role": "agent", "parts": []map[string]any{{"kind": "data", "data": data}}},
		},
	}
}

func taskEvent(taskID, ctxID, state string) map[string]any {
	return map[string]any{
		"kind":      "task",
		"id":        taskID,
		"contextId": ctxID,
		"status":    map[string]any{"state": state},
	}
}

func randHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
