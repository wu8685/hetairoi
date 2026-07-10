// Package config loads hetairoi configuration from the environment.
package config

import (
	"os"
	"strings"
)

type Config struct {
	// Listen is the hetairoi HTTP bind address (the eventbus control plane).
	Listen string
	// APIKeys is the set of accepted x-api-key values. Empty => allow all
	// (local/zero-config).
	APIKeys map[string]bool
	// StateFile's directory is where the eventbus persists sources/handlers and
	// per-handler dedup state across restarts.
	StateFile string
}

func Load() Config {
	c := Config{
		Listen:    env("CMA_LISTEN", ":8787"),
		StateFile: env("CMA_STATE_FILE", "cma-state.json"),
		APIKeys:   map[string]bool{},
	}
	for _, k := range strings.Split(os.Getenv("CMA_API_KEYS"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.APIKeys[k] = true
		}
	}
	return c
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// This file defines Hetairoi's runtime configuration.
