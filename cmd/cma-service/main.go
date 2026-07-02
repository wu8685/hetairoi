// Command cma-service exposes an Anthropic Managed Agents (CMA) compatible API
// backed by an ahsir agent fleet.
package main

import (
	"context"
	"log"
	"net/http"
	"path/filepath"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/api"
	"github.com/wu8685/cma-service/internal/config"
	"github.com/wu8685/cma-service/internal/eventbus"
	"github.com/wu8685/cma-service/internal/store"
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
	bus := eventbus.New(srv.BusDriver(), busDir, 8)
	reg, err := eventbus.NewRegistry(context.Background(), bus, busDir)
	if err != nil {
		log.Fatalf("event bus registry: %v", err)
	}
	srv.SetEventRegistry(reg)

	log.Printf("cma-service listening on %s (ahsir=%s, state=%s)", cfg.Listen, cfg.AhsirURL, cfg.StateFile)
	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
