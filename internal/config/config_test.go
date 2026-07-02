package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAhsirAdminToken(t *testing.T) {
	// Isolate from the developer's real env/home for the file-discovery cases.
	t.Setenv("CMA_AHSIR_ADMIN_TOKEN", "")
	t.Setenv("AHSIR_ADMIN_TOKEN", "")

	dir := t.TempDir()
	cfg := filepath.Join(dir, "ahsir.yaml")
	if err := os.WriteFile(filepath.Join(dir, "admin-token"), []byte("file-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("explicit env wins", func(t *testing.T) {
		t.Setenv("CMA_AHSIR_ADMIN_TOKEN", "explicit")
		t.Setenv("AHSIR_ADMIN_TOKEN", "ahsir-env")
		t.Setenv("CMA_AHSIR_CONFIG", cfg)
		if got := resolveAhsirAdminToken(); got != "explicit" {
			t.Errorf("got %q, want explicit", got)
		}
	})

	t.Run("ahsir env over file", func(t *testing.T) {
		t.Setenv("CMA_AHSIR_ADMIN_TOKEN", "")
		t.Setenv("AHSIR_ADMIN_TOKEN", "ahsir-env")
		t.Setenv("CMA_AHSIR_CONFIG", cfg)
		if got := resolveAhsirAdminToken(); got != "ahsir-env" {
			t.Errorf("got %q, want ahsir-env", got)
		}
	})

	t.Run("admin-token file beside config, trimmed", func(t *testing.T) {
		t.Setenv("CMA_AHSIR_ADMIN_TOKEN", "")
		t.Setenv("AHSIR_ADMIN_TOKEN", "")
		t.Setenv("CMA_AHSIR_CONFIG", cfg)
		if got := resolveAhsirAdminToken(); got != "file-tok" {
			t.Errorf("got %q, want file-tok", got)
		}
	})

	t.Run("no token when nothing configured", func(t *testing.T) {
		t.Setenv("CMA_AHSIR_ADMIN_TOKEN", "")
		t.Setenv("AHSIR_ADMIN_TOKEN", "")
		t.Setenv("CMA_AHSIR_CONFIG", filepath.Join(dir, "nonexistent", "ahsir.yaml"))
		if got := resolveAhsirAdminToken(); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
