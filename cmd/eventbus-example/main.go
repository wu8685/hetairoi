// Command eventbus-example runs cma-service with an event bus wired for an
// ops-alert auto-handling scenario: a webhook receives alerts, a handler routes
// each alert (by subject) to a session, and an ahsir-backed agent investigates
// using its shell tools and reports.
//
// Flow:
//
//	POST /eventbus/events {type:"alert", subject, payload}
//	  → handler "ops-alerts" (Keyed: one session per subject/incident)
//	  → prompt the ops-responder agent → it runs real commands → reports
//
// Run it behind a real ahsir + provider; see example/eventbus/run.sh.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/api"
	"github.com/wu8685/cma-service/internal/cma"
	"github.com/wu8685/cma-service/internal/config"
	"github.com/wu8685/cma-service/internal/eventbus"
	"github.com/wu8685/cma-service/internal/store"
)

func main() {
	cfg := config.Load()

	st, err := store.New(cfg.StateFile)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	ac := ahsir.New(cfg.AhsirURL, cfg.AhsirAdminToken)
	srv := api.New(cfg, st, ac)

	model := env("EXAMPLE_MODEL", "deepseek-chat")
	agentID := seedAgent(st, "ops-responder", model,
		"You are an on-call SRE assistant. When given an alert, run AT MOST 2 shell commands with "+
			"your tools to diagnose it (e.g. df, ls), then STOP and give a 2-3 sentence summary of the "+
			"finding and any remediation. Be fast and decisive — do not keep investigating. Use real "+
			"command output, do not make things up.")
	envID := seedEnv(st, "ops")
	log.Printf("seeded agent=%s env=%s model=%s", agentID, envID, model)

	bus := eventbus.New(srv.BusDriver(), filepath.Dir(cfg.StateFile), 8)
	err = bus.Register(eventbus.Subscription{
		Name:  "ops-alerts",
		Match: func(e eventbus.Event) bool { return e.Type == "alert" },
		// Keyed: one session per alert subject — related alerts for the same
		// incident accumulate in one conversation the agent (and a human) can
		// follow.
		Policy: eventbus.Keyed{
			Agent: eventbus.AgentRef{ID: agentID},
			EnvID: envID,
			Key:   func(e eventbus.Event) string { return e.Subject },
			Prompt: func(e eventbus.Event) string {
				return fmt.Sprintf("Alert on %q:\n%s\n\nInvestigate with your tools and report.",
					e.Subject, string(e.Payload))
			},
		},
	})
	if err != nil {
		log.Fatalf("register handler: %v", err)
	}
	srv.SetEventBus(bus)

	log.Printf("eventbus-example on %s — POST /eventbus/events {\"id\",\"type\":\"alert\",\"subject\",\"payload\"}", cfg.Listen)
	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func seedAgent(st *store.Store, name, model, system string) string {
	now := time.Now().UTC()
	a := &cma.Agent{
		Type: "agent", ID: cma.NewID(cma.PrefixAgent), Version: 1, Name: name,
		Model: cma.ModelConfig{ID: model}, System: system,
		Tools: []cma.ToolDef{}, Skills: []cma.SkillRef{}, MCPServers: []cma.MCPServer{},
		Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.PutAgentVersion(a); err != nil {
		log.Fatalf("seed agent: %v", err)
	}
	return a.ID
}

func seedEnv(st *store.Store, name string) string {
	now := time.Now().UTC()
	e := &cma.Environment{
		Type: "environment", ID: cma.NewID(cma.PrefixEnvironment), Name: name,
		Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.PutEnvironment(e); err != nil {
		log.Fatalf("seed env: %v", err)
	}
	return e.ID
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
