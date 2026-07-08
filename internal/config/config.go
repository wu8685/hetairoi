// Package config loads hetairoi configuration from the environment.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	// Listen is the hetairoi HTTP bind address.
	Listen string
	// AhsirURL is the ahsir scheduler gateway root.
	AhsirURL string
	// AhsirAdminToken authenticates control-plane calls to ahsir.
	AhsirAdminToken string
	// APIKeys is the set of accepted x-api-key values. Empty => allow all
	// (local/zero-config, mirrors ahsir's degenerate case).
	APIKeys map[string]bool
	// StateFile persists the resource store across restarts.
	StateFile string

	// Runtime* supply the provider credentials baked into every ahsir agent
	// card. The CMA `model` id maps to Runtime.Model; provider/baseURL/apiKey
	// come from here so callers never send infra secrets over the CMA API.
	RuntimeProvider string
	RuntimeBaseURL  string
	RuntimeAPIKey   string

	// TurnTimeout caps a single agent turn. The 10m default suits chat-style
	// turns; heavy event-driven agents (clone + build/test + push) need more —
	// raise it with CMA_TURN_TIMEOUT (Go duration, e.g. "30m").
	TurnTimeout time.Duration
}

func Load() Config {
	c := Config{
		Listen:          env("CMA_LISTEN", ":8787"),
		AhsirURL:        env("CMA_AHSIR_URL", "http://127.0.0.1:9800"),
		AhsirAdminToken: resolveAhsirAdminToken(),
		StateFile:       env("CMA_STATE_FILE", "cma-state.json"),
		RuntimeProvider: os.Getenv("CMA_RUNTIME_PROVIDER"),
		RuntimeBaseURL:  os.Getenv("CMA_RUNTIME_BASE_URL"),
		RuntimeAPIKey:   os.Getenv("CMA_RUNTIME_API_KEY"),
		TurnTimeout:     durEnv("CMA_TURN_TIMEOUT", 10*time.Minute),
		APIKeys:         map[string]bool{},
	}
	for _, k := range strings.Split(os.Getenv("CMA_API_KEYS"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.APIKeys[k] = true
		}
	}
	return c
}

// resolveAhsirAdminToken discovers the ahsir control-plane token the same way
// the ahsir CLI does, so a local same-user hetairoi needs zero token wiring:
//
//	1. CMA_AHSIR_ADMIN_TOKEN  — explicit override
//	2. AHSIR_ADMIN_TOKEN      — the env the scheduler itself reads
//	3. admin-token file beside the ahsir config (CMA_AHSIR_CONFIG, default
//	   ~/.ahsir/ahsir.yaml → ~/.ahsir/admin-token) — what `ahsir start` writes
//
// Empty result means token-free (the scheduler's auth-disabled degenerate case).
func resolveAhsirAdminToken() string {
	if v := os.Getenv("CMA_AHSIR_ADMIN_TOKEN"); v != "" {
		return v
	}
	if v := os.Getenv("AHSIR_ADMIN_TOKEN"); v != "" {
		return v
	}
	cfgPath := os.Getenv("CMA_AHSIR_CONFIG")
	if cfgPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfgPath = filepath.Join(home, ".ahsir", "ahsir.yaml")
		}
	}
	if cfgPath != "" {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), "admin-token")); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func durEnv(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
