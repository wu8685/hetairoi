package ahsir

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Client talks to the ahsir scheduler gateway (default http://127.0.0.1:9800).
type Client struct {
	BaseURL    string
	AdminToken string
	HTTP       *http.Client
}

// New builds a client. baseURL is the scheduler gateway root.
//
// The HTTP client has NO fixed Timeout: it would cap the whole request/response
// including a streaming ChatStream turn, cutting long agent turns short (a
// clone+build+test fixer needs far more than a few minutes). Every call instead
// passes a context with its own deadline (e.g. the per-turn CMA_TURN_TIMEOUT), so
// the context governs the duration, not a blanket client timeout.
func New(baseURL, adminToken string) *Client {
	return &Client{
		BaseURL:    baseURL,
		AdminToken: adminToken,
		HTTP:       &http.Client{},
	}
}

// registerRequest is the body for POST /admin/agents. The `card` field is the
// proposed inline-registration extension (pending ahsir-side support); until it
// lands, RegisterAgent will fail and the gateway surfaces a clear error.
type registerRequest struct {
	Name      string     `json:"name"`
	Workspace string     `json:"workspace,omitempty"`
	Card      *AgentCard `json:"card,omitempty"`
}

// RegisterAgent hot-registers an ahsir agent from an inline card. Idempotent at
// the caller level: a 409 "already running" means the (versioned) agent exists,
// which we treat as success.
func (c *Client) RegisterAgent(ctx context.Context, name string, card *AgentCard) error {
	body, err := json.Marshal(registerRequest{Name: name, Card: card})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/admin/agents", body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("register agent %q: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return nil // already running — fine for our versioned-name model
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// 401 here almost always means one of: CMA_AHSIR_ADMIN_TOKEN doesn't match
		// the scheduler's token, OR CMA_AHSIR_URL points at the wrong process (e.g.
		// an agent's A2A port instead of the scheduler, common when a port collides
		// with a running fleet). Surface both so the operator isn't left guessing.
		return fmt.Errorf("register agent %q: 401 unauthorized from %s — check CMA_AHSIR_ADMIN_TOKEN "+
			"matches the scheduler's admin token (or that both are unset for token-free local use), "+
			"and that CMA_AHSIR_URL points at the ahsir scheduler, not an agent port", name, c.BaseURL)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("register agent %q: %s", name, readErr(resp))
	}
	return nil
}

// DeleteAgent stops a running ahsir agent (workspace files preserved).
func (c *Client) DeleteAgent(ctx context.Context, name string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/admin/agents/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete agent %q: %s", name, readErr(resp))
	}
	return nil
}

type chatRequest struct {
	Message   string `json:"message"`
	ContextID string `json:"contextId,omitempty"`
	Speaker   string `json:"speaker,omitempty"`
}

type chatResponse struct {
	Response string `json:"response"`
}

// Chat sends one turn synchronously and returns the aggregated reply. This is
// the MVP path behind the CMA event stream (one agent.message + idle). Token-
// incremental streaming via A2A message/stream is a fast-follow once the wire
// shape is verified against ahsir.
func (c *Client) Chat(ctx context.Context, agent, contextID, speaker, message string) (string, error) {
	body, err := json.Marshal(chatRequest{Message: message, ContextID: contextID, Speaker: speaker})
	if err != nil {
		return "", err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/agents/"+url.PathEscape(agent)+"/chat", body)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat %q: %w", agent, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("chat %q: %s", agent, readErr(resp))
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("chat %q: decode: %w", agent, err)
	}
	return out.Response, nil
}

// TranscriptTurn is one entry of ahsir's history endpoint. Field names are best
// effort against GET /agents/{name}/history/{contextId}; verify and tighten.
type TranscriptTurn struct {
	Speaker string `json:"speaker,omitempty"`
	Role    string `json:"role,omitempty"`
	Text    string `json:"text,omitempty"`
}

// History returns the transcript for a context, used to back GET .../events
// (history replay + dedupe).
func (c *Client) History(ctx context.Context, agent, contextID string) ([]TranscriptTurn, error) {
	path := "/agents/" + url.PathEscape(agent) + "/history/" + url.PathEscape(contextID)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("history %q: %s", agent, readErr(resp))
	}
	var turns []TranscriptTurn
	if err := json.NewDecoder(resp.Body).Decode(&turns); err != nil {
		return nil, fmt.Errorf("history %q: decode: %w", agent, err)
	}
	return turns, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.AdminToken != "" {
		req.Header.Set("X-Ahsir-Admin-Token", c.AdminToken)
	}
	return req, nil
}

func readErr(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if len(b) == 0 {
		return resp.Status
	}
	return fmt.Sprintf("%s: %s", resp.Status, bytes.TrimSpace(b))
}
