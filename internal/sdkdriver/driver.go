// Package sdkdriver implements eventbus.SessionDriver by driving ahsir's CMA API
// through the official anthropic-sdk-go. It makes hetairoi a CMA *client* of
// ahsir (migration P2, decision B1: dogfood the same wire the SDK certifies)
// instead of reaching into an in-process gateway.
package sdkdriver

import (
	"context"
	"fmt"
	"strings"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/wu8685/hetairoi/internal/eventbus"
)

const summaryTrunc = 200

// Driver talks the CMA wire to a base URL (ahsir's CMA facade) via the official
// SDK. Zero LLM knowledge — it just creates sessions, sends user messages, and
// reads back the event log.
type Driver struct {
	client  anthropic.Client
	timeout time.Duration
}

// New builds a Driver pointed at the CMA facade base URL. apiKey may be any
// value locally (the facade's x-api-key allowlist is empty for local use).
func New(baseURL, apiKey string) *Driver {
	if apiKey == "" {
		apiKey = "sk-cma-eventbus"
	}
	return &Driver{
		client:  anthropic.NewClient(option.WithBaseURL(baseURL), option.WithAPIKey(apiKey)),
		timeout: 10 * time.Minute,
	}
}

var _ eventbus.SessionDriver = (*Driver)(nil)

func agentParam(a eventbus.AgentRef) anthropic.BetaSessionNewParamsAgentUnion {
	// The id string pins the latest version; the eventbus uses Version 0 (latest)
	// in practice. (Explicit version-pinning would use OfBetaManagedAgentsAgents.)
	return anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(a.ID)}
}

func userMessageEvent(text string) anthropic.BetaManagedAgentsEventParamsUnion {
	return anthropic.BetaManagedAgentsEventParamsUnion{
		OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
			Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
				OfText: &anthropic.BetaManagedAgentsTextBlockParam{
					Text: text,
					Type: anthropic.BetaManagedAgentsTextBlockTypeText,
				},
			}},
			Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
		},
	}
}

func (d *Driver) CreateSession(agent eventbus.AgentRef, envID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := d.client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         agentParam(agent),
		EnvironmentID: envID,
	})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return s.ID, nil
}

func (d *Driver) SendUserMessage(sessionID, prompt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := d.client.Beta.Sessions.Events.Send(ctx, sessionID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{userMessageEvent(prompt)},
	})
	if err != nil {
		return fmt.Errorf("send user message: %w", err)
	}
	return nil
}

// RunForReply drives a one-shot stateless turn: a throwaway session, one message,
// then poll the persisted event log until the turn goes idle and return the
// accumulated agent text. Polling the log (rather than the live SSE tail) keeps
// it deterministic against a fast runtime.
func (d *Driver) RunForReply(agent eventbus.AgentRef, envID, prompt string) (string, error) {
	sid, err := d.CreateSession(agent, envID)
	if err != nil {
		return "", err
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_, _ = d.client.Beta.Sessions.Delete(dctx, sid, anthropic.BetaSessionDeleteParams{})
	}()

	if err := d.SendUserMessage(sid, prompt); err != nil {
		return "", err
	}

	deadline := time.Now().Add(d.timeout)
	for time.Now().Before(deadline) {
		reply, done, err := d.readTurn(sid)
		if err != nil {
			return "", err
		}
		if done {
			return reply, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", fmt.Errorf("run-for-reply: session %s did not go idle within %s", sid, d.timeout)
}

// readTurn lists the session's events and returns the accumulated agent.message
// text plus whether the turn has completed (a session.status_idle is present).
func (d *Driver) readTurn(sessionID string) (reply string, done bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var sb strings.Builder
	iter := d.client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, anthropic.BetaSessionEventListParams{})
	for iter.Next() {
		ev := iter.Current()
		switch ev.Type {
		case "agent.message":
			for _, b := range ev.AsAgentMessage().Content {
				sb.WriteString(b.Text)
			}
		case "session.status_idle":
			done = true
		case "session.status_terminated", "session.error":
			return sb.String(), true, fmt.Errorf("session %s ended: %s", sessionID, ev.Type)
		}
	}
	if err := iter.Err(); err != nil {
		return "", false, fmt.Errorf("list events: %w", err)
	}
	return sb.String(), done, nil
}

func (d *Driver) SessionSummary(sessionID string) (eventbus.SessionSummary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := d.client.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
	if err != nil {
		return eventbus.SessionSummary{}, fmt.Errorf("get session: %w", err)
	}
	out := eventbus.SessionSummary{
		SessionID: sessionID,
		CreatedAt: sess.CreatedAt,
		Archived:  !sess.ArchivedAt.IsZero(),
	}

	var seed, last string
	var lastActive time.Time
	iter := d.client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, anthropic.BetaSessionEventListParams{})
	for iter.Next() {
		ev := iter.Current()
		if !ev.ProcessedAt.IsZero() {
			lastActive = ev.ProcessedAt // events come oldest-first
		}
		switch ev.Type {
		case "user.message":
			if seed == "" {
				for _, b := range ev.AsUserMessage().Content {
					seed += b.Text
				}
			}
		case "agent.message":
			last = ""
			for _, b := range ev.AsAgentMessage().Content {
				last += b.Text
			}
		}
	}
	if err := iter.Err(); err != nil {
		return eventbus.SessionSummary{}, fmt.Errorf("list events: %w", err)
	}
	out.Seed = truncate(seed, summaryTrunc)
	out.Last = truncate(last, summaryTrunc)
	if !lastActive.IsZero() {
		out.LastActiveAt = lastActive
	} else {
		out.LastActiveAt = sess.CreatedAt
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
