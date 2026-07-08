package ahsir

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrTurnCanceled is returned by ChatStream when the turn ended because it was
// cancelled (via CancelTask / tasks/cancel) rather than completing normally.
var ErrTurnCanceled = errors.New("turn canceled")

// These minimal types mirror the a2a-go wire shapes (github.com/a2aproject/
// a2a-go/a2a) for exactly the fields hetairoi produces/consumes — kept
// hand-rolled so hetairoi stays stdlib-only. Verified against a2a-go v0.3.15.

type a2aTextPart struct {
	Kind string `json:"kind"` // "text"
	Text string `json:"text"`
}

type a2aMessage struct {
	MessageID string        `json:"messageId"`
	Role      string        `json:"role"` // "user"
	Parts     []a2aTextPart `json:"parts"`
	ContextID string        `json:"contextId,omitempty"`
}

type a2aMessageSendParams struct {
	Message a2aMessage `json:"message"`
}

type a2aTaskIDParams struct {
	ID string `json:"id"`
}

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      string `json:"id"`
}

// jsonrpcStreamResponse is one SSE frame's payload: a JSON-RPC response whose
// result is an A2A event (status-update or task), discriminated by `kind`.
type jsonrpcStreamResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *jsonrpcError   `json:"error"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// a2aPart is a response message part: a TextPart (incremental text) or a
// DataPart (a structured observability event — tool_use, span, etc.).
type a2aPart struct {
	Kind string                     `json:"kind"` // "text" | "data"
	Text string                     `json:"text"`
	Data map[string]json.RawMessage `json:"data"`
}

// a2aEvent is the union of the streamed event shapes we care about. A
// status-update carries text deltas and/or structured DataParts in
// status.message.parts; the terminal "task" event carries the final state.
type a2aEvent struct {
	Kind   string `json:"kind"` // "status-update" | "task"
	TaskID string `json:"taskId"`
	ID     string `json:"id"` // present on "task" events
	Final  bool   `json:"final"`
	Status struct {
		State   string `json:"state"` // working | completed | failed | canceled | ...
		Message *struct {
			Parts []a2aPart `json:"parts"`
		} `json:"message"`
	} `json:"status"`
}

// StreamEvent is a structured (non-text) event the agent surfaces mid-turn,
// carried as an A2A DataPart. It is a neutral carrier — hetairoi maps it to
// the CMA observability events (tool_use / mcp_tool_use / span.*). Kind is the
// DataPart's "ev" discriminator.
type StreamEvent struct {
	Kind  string          // "tool_use" | "thinking" | "tool_result" | "span_start" | "span_end"
	ID    string          // tool-use id (tool_use)
	Name  string          // tool name (tool_use)
	Input json.RawMessage // tool input (tool_use)
	Usage *StreamUsage    // span_end

	// tool_result:
	ToolUseID string // the tool_use this result is for
	Content   string // flattened result text
	IsError   bool
}

// StreamUsage is the token usage carried on a span_end StreamEvent.
type StreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parseDataPart decodes a DataPart's data map into a StreamEvent. Returns false
// if it has no recognizable "ev" discriminator.
func parseDataPart(data map[string]json.RawMessage) (StreamEvent, bool) {
	evRaw, ok := data["ev"]
	if !ok {
		return StreamEvent{}, false
	}
	var ev StreamEvent
	_ = json.Unmarshal(evRaw, &ev.Kind)
	if r, ok := data["id"]; ok {
		_ = json.Unmarshal(r, &ev.ID)
	}
	if r, ok := data["name"]; ok {
		_ = json.Unmarshal(r, &ev.Name)
	}
	if r, ok := data["input"]; ok {
		ev.Input = r
	}
	if r, ok := data["usage"]; ok {
		var u StreamUsage
		if json.Unmarshal(r, &u) == nil {
			ev.Usage = &u
		}
	}
	if r, ok := data["tool_use_id"]; ok {
		_ = json.Unmarshal(r, &ev.ToolUseID)
	}
	if r, ok := data["content"]; ok {
		_ = json.Unmarshal(r, &ev.Content)
	}
	if r, ok := data["is_error"]; ok {
		_ = json.Unmarshal(r, &ev.IsError)
	}
	return ev, ev.Kind != ""
}

// ChatStream drives one turn over the A2A `message/stream` transport (proxied
// by the scheduler at POST /a2a/{name}). It accumulates the agent's incremental
// text deltas and returns the aggregated reply. onTaskID, if non-nil, is
// invoked once as soon as the A2A task id is known (from the first event) so
// the caller can wire cancellation while the turn is still in flight.
//
// Returns ErrTurnCanceled if the turn ended in the canceled state.
func (c *Client) ChatStream(ctx context.Context, agent, contextID, message string, onTaskID func(string), onReschedule func(), onEvent func(StreamEvent)) (string, error) {
	params := a2aMessageSendParams{Message: a2aMessage{
		MessageID: randID(),
		Role:      "user",
		Parts:     []a2aTextPart{{Kind: "text", Text: message}},
		ContextID: contextID,
	}}
	body, err := json.Marshal(jsonrpcRequest{JSONRPC: "2.0", Method: "message/stream", Params: params, ID: randID()})
	if err != nil {
		return "", err
	}
	resp, err := c.openStream(ctx, agent, body, onReschedule)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var (
		buf          strings.Builder
		taskIDSeen   bool
		canceled     bool
		streamFailed string
	)
	noteTaskID := func(id string) {
		if id != "" && !taskIDSeen {
			taskIDSeen = true
			if onTaskID != nil {
				onTaskID(id)
			}
		}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // deltas can be large
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // SSE comments (": ping"), event: lines, blank separators
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" {
			continue
		}
		var frame jsonrpcStreamResponse
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue // tolerate non-JSON keepalive frames
		}
		if frame.Error != nil {
			return "", fmt.Errorf("stream %q: rpc error %d: %s", agent, frame.Error.Code, frame.Error.Message)
		}
		var ev a2aEvent
		if err := json.Unmarshal(frame.Result, &ev); err != nil {
			continue
		}
		noteTaskID(ev.TaskID)
		noteTaskID(ev.ID)

		switch ev.Kind {
		case "status-update":
			if ev.Status.Message != nil {
				for _, p := range ev.Status.Message.Parts {
					switch p.Kind {
					case "text":
						buf.WriteString(p.Text)
					case "data":
						if se, ok := parseDataPart(p.Data); ok && onEvent != nil {
							onEvent(se)
						}
					}
				}
			}
		case "task":
			// Terminal event. Adopt its final message text if no deltas were
			// streamed (partial_messages off, or a non-delta runtime).
			if buf.Len() == 0 && ev.Status.Message != nil {
				for _, p := range ev.Status.Message.Parts {
					if p.Kind == "text" {
						buf.WriteString(p.Text)
					}
				}
			}
			switch ev.Status.State {
			case "canceled":
				canceled = true
			case "failed", "rejected":
				streamFailed = ev.Status.State
			}
		}
	}
	if err := sc.Err(); err != nil {
		return buf.String(), fmt.Errorf("stream %q: read: %w", agent, err)
	}
	if canceled {
		return buf.String(), ErrTurnCanceled
	}
	if streamFailed != "" {
		return buf.String(), fmt.Errorf("stream %q: task %s", agent, streamFailed)
	}
	return buf.String(), nil
}

// openStream POSTs the message/stream request and returns the live SSE
// response, retrying transient pre-stream failures. Right after registration
// the agent's A2A server may still be binding its port (the admin start returns
// at spawn, not at listen), and a supervised restart briefly drops the port;
// both surface as a dial refusal or a 502 from the gateway proxy. No turn has
// started in those cases, so retrying is idempotent.
func (c *Client) openStream(ctx context.Context, agent string, body []byte, onReschedule func()) (*http.Response, error) {
	const maxAttempts = 10
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if attempt == 1 && onReschedule != nil {
				// First retry: the agent was unreachable and we're waiting for
				// it to come back — i.e. its compute is being (re)scheduled.
				onReschedule()
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
			}
		}
		req, err := c.newRequest(ctx, http.MethodPost, "/a2a/"+url.PathEscape(agent), body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/event-stream")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("stream %q: %w", agent, err) // dial refused etc.
			continue
		}
		if resp.StatusCode == http.StatusBadGateway {
			lastErr = fmt.Errorf("stream %q: %s", agent, readErr(resp)) // proxy couldn't reach agent yet
			resp.Body.Close()
			continue
		}
		if resp.StatusCode >= 300 {
			err := fmt.Errorf("stream %q: %s", agent, readErr(resp))
			resp.Body.Close()
			return nil, err
		}
		return resp, nil
	}
	return nil, fmt.Errorf("stream %q: agent unreachable after %d attempts: %w", agent, maxAttempts, lastErr)
}

// CancelTask requests cancellation of an in-flight A2A task (tasks/cancel),
// backing CMA's user.interrupt. Best-effort: a task that already finished or is
// unknown is not an error the caller needs to act on.
func (c *Client) CancelTask(ctx context.Context, agent, taskID string) error {
	body, err := json.Marshal(jsonrpcRequest{JSONRPC: "2.0", Method: "tasks/cancel", Params: a2aTaskIDParams{ID: taskID}, ID: randID()})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/a2a/"+url.PathEscape(agent), body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cancel %q/%s: %w", agent, taskID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("cancel %q/%s: %s", agent, taskID, readErr(resp))
	}
	// Drain so the connection can be reused.
	_, _ = bytes.NewBuffer(nil).ReadFrom(resp.Body)
	return nil
}

func randID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
