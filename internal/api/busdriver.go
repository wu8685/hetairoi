package api

import (
	"context"
	"fmt"
	"time"

	"github.com/wu8685/hetairoi/internal/cma"
	"github.com/wu8685/hetairoi/internal/eventbus"
	"github.com/wu8685/hetairoi/internal/store"
	"github.com/wu8685/hetairoi/internal/translate"
)

// deliverUserMessage records the inbound user message in the log (it's part of
// the conversation history events.list returns) and drives the turn. Shared by
// the events.send handler and the event bus driver.
func (s *Server) deliverUserMessage(rec *store.SessionRecord, content []cma.ContentBlock) {
	um := newEvent(cma.EvtUserMessage)
	um.Content = content
	s.emit(rec, um)
	s.runTurn(rec, textOf(content))
}

// runOneShot drives a single stateless turn (fresh contextId, no persistent
// session) and returns the agent's final text. Backs the event bus's Routed
// router decision.
func (s *Server) runOneShot(ctx context.Context, ref cma.AgentRef, prompt string) (string, error) {
	agent, ok := s.store.Agent(ref.ID, ref.Version)
	if !ok {
		return "", errAgentNotFound
	}
	ahsirName := translate.AhsirAgentName(agent.ID, agent.Version)
	if err := s.ensureRegistered(ctx, ahsirName, agent); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return s.ahsir.ChatStream(ctx, ahsirName, cma.NewID("ctx"), prompt, nil, nil, nil)
}

// sessionSummary derives a compact summary from a session's event log: the first
// user.message is the topic seed, the last agent.message is the current state.
func (s *Server) sessionSummary(sessionID string) (eventbus.SessionSummary, error) {
	rec, ok := s.store.Session(sessionID)
	if !ok {
		return eventbus.SessionSummary{}, fmt.Errorf("session %s not found", sessionID)
	}
	sum := eventbus.SessionSummary{
		SessionID: sessionID,
		CreatedAt: rec.Session.CreatedAt,
		Archived:  rec.Session.ArchivedAt != nil,
	}
	for _, ev := range rec.Snapshot() {
		switch ev.Type {
		case cma.EvtUserMessage:
			if sum.Seed == "" {
				sum.Seed = truncate(textOf(ev.Content), 200)
			}
		case cma.EvtAgentMessage:
			sum.Last = truncate(textOf(ev.Content), 200)
		}
		if ev.ProcessedAt != nil {
			sum.LastActiveAt = *ev.ProcessedAt
		}
	}
	return sum, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// busDriver adapts *Server to eventbus.SessionDriver so the event bus can drive
// sessions in-process.
type busDriver struct{ srv *Server }

// BusDriver returns an eventbus.SessionDriver backed by this server.
func (s *Server) BusDriver() eventbus.SessionDriver { return busDriver{s} }

func (d busDriver) CreateSession(agent eventbus.AgentRef, envID string) (string, error) {
	rec, err := d.srv.createSessionRecord(context.Background(),
		cma.AgentRef{Type: "agent", ID: agent.ID, Version: agent.Version}, envID, "", nil)
	if err != nil {
		return "", err
	}
	return rec.Session.ID, nil
}

func (d busDriver) SendUserMessage(sessionID, prompt string) error {
	rec, ok := d.srv.store.Session(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	d.srv.deliverUserMessage(rec, []cma.ContentBlock{{Type: "text", Text: prompt}})
	return nil
}

func (d busDriver) RunForReply(agent eventbus.AgentRef, envID, prompt string) (string, error) {
	return d.srv.runOneShot(context.Background(),
		cma.AgentRef{Type: "agent", ID: agent.ID, Version: agent.Version}, prompt)
}

func (d busDriver) SessionSummary(sessionID string) (eventbus.SessionSummary, error) {
	return d.srv.sessionSummary(sessionID)
}
