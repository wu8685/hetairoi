// Command hetairoi exposes an Anthropic Managed Agents (CMA) compatible API
// backed by an ahsir agent fleet.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/wu8685/hetairoi/internal/ahsir"
	"github.com/wu8685/hetairoi/internal/api"
	"github.com/wu8685/hetairoi/internal/config"
	"github.com/wu8685/hetairoi/internal/eventbus"
	"github.com/wu8685/hetairoi/internal/sdkdriver"
	"github.com/wu8685/hetairoi/internal/store"
)

func main() {
	cfg := config.Load()

	st, err := store.New(cfg.StateFile)
	if err != nil {
		log.Fatalf("load store: %v", err)
	}

	ac := ahsir.New(cfg.AhsirURL, cfg.AhsirAdminToken)
	srv := api.New(cfg, st, ac)

	// Mount the event-bus control plane: POST/GET/DELETE /v1/eventbus/{sources,
	// handlers} let an operator wire CodeHub/workitem event monitoring at runtime;
	// persisted specs are rebuilt here on boot.
	busDir := filepath.Dir(cfg.StateFile)

	// Driver selection (migration P2): default is the in-process gateway
	// (BusDriver). Opt into the CMA-SDK client by setting CMA_EVENTBUS_DRIVER=sdk
	// + CMA_FACADE_URL — the eventbus then drives ahsir's CMA facade through the
	// official anthropic-sdk-go, exactly as an external CMA client would.
	var driver eventbus.SessionDriver = srv.BusDriver()
	if os.Getenv("CMA_EVENTBUS_DRIVER") == "sdk" {
		facadeURL := os.Getenv("CMA_FACADE_URL")
		if facadeURL == "" {
			log.Fatalf("CMA_EVENTBUS_DRIVER=sdk requires CMA_FACADE_URL (the ahsir CMA facade base URL)")
		}
		driver = sdkdriver.New(facadeURL, os.Getenv("CMA_API_KEY"))
		srv.SetExternalAgents(true) // agents live on the facade, not the local store
		log.Printf("eventbus driver: sdk (facade=%s)", facadeURL)
	}
	bus := eventbus.New(driver, busDir, 8)
	reg, err := eventbus.NewRegistry(context.Background(), bus, busDir)
	if err != nil {
		log.Fatalf("event bus registry: %v", err)
	}
	srv.SetEventRegistry(reg)

	log.Printf("hetairoi listening on %s (ahsir=%s, state=%s)", cfg.Listen, cfg.AhsirURL, cfg.StateFile)
	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
