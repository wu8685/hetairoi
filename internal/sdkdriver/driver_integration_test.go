package sdkdriver_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/wu8685/hetairoi/internal/eventbus"
	"github.com/wu8685/hetairoi/internal/sdkdriver"
)

// TestIntegrationSDKDriver boots a real ahsir (scheduler + CMA facade, echo
// provider) and drives it through the SDK-backed eventbus.SessionDriver — the
// migration P2 dogfood: hetairoi talking CMA to ahsir via the official SDK.
//
// Skipped under -short or when the ahsir repo isn't present. The ahsir binaries
// are built from the sibling ahsir checkout (branch feat/cma-facade) and
// codesigned (this box SIGKILLs unsigned freshly-built binaries).
func TestIntegrationSDKDriver(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: needs a live ahsir build")
	}
	ahsirRepo := ahsirRepoPath(t)
	if _, err := os.Stat(filepath.Join(ahsirRepo, "cmd", "ahsir")); err != nil {
		t.Skipf("ahsir repo not found at %s", ahsirRepo)
	}

	facadeURL := bootFacade(t, ahsirRepo)

	// Create an agent (echo, via the facade's CMA_RUNTIME_PROVIDER default) + env.
	client := anthropic.NewClient(option.WithBaseURL(facadeURL), option.WithAPIKey("sk-it"))
	ctx := context.Background()
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name:  "it-echo",
		Model: anthropic.BetaManagedAgentsModelConfigParams{ID: anthropic.BetaManagedAgentsModel("claude-opus-4-8")},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name:   "it-default",
		Config: anthropic.BetaEnvironmentNewParamsConfigUnion{OfCloud: &anthropic.BetaCloudConfigParams{}},
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}

	d := sdkdriver.New(facadeURL, "sk-it")
	ref := eventbus.AgentRef{ID: agent.ID}

	// CreateSession
	sid, err := d.CreateSession(ref, env.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !strings.HasPrefix(sid, "sesn_") {
		t.Fatalf("CreateSession: unexpected id %q", sid)
	}

	// SendUserMessage + poll the log until the turn goes idle.
	if err := d.SendUserMessage(sid, "hello via sdk"); err != nil {
		t.Fatalf("SendUserMessage: %v", err)
	}
	sum := waitForReply(t, d, sid)
	// Seed is the (short, untruncated) first user.message → the prompt verbatim.
	if !strings.Contains(sum.Seed, "hello via sdk") {
		t.Fatalf("SessionSummary.Seed = %q, want the first user message", sum.Seed)
	}
	// Last is the truncated agent.message; the echo prefix proves it's the reply.
	// (ahsir prepends multiagent boilerplate, so the echoed prompt text can be
	// past the 200-char truncation — RunForReply below asserts the full round-trip.)
	if !strings.HasPrefix(sum.Last, "echo:") {
		t.Fatalf("SessionSummary.Last = %q, want the echoed agent reply", sum.Last)
	}

	// RunForReply (one-shot stateless turn)
	reply, err := d.RunForReply(ref, env.ID, "one shot please")
	if err != nil {
		t.Fatalf("RunForReply: %v", err)
	}
	if !strings.Contains(reply, "one shot please") {
		t.Fatalf("RunForReply = %q, want it to contain the prompt (echo)", reply)
	}
}

func waitForReply(t *testing.T, d *sdkdriver.Driver, sid string) eventbus.SessionSummary {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		sum, err := d.SessionSummary(sid)
		if err != nil {
			t.Fatalf("SessionSummary: %v", err)
		}
		if sum.Last != "" {
			return sum
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("no agent reply within 30s")
	return eventbus.SessionSummary{}
}

// ----- harness -----

func ahsirRepoPath(t *testing.T) string {
	if p := os.Getenv("AHSIR_REPO"); p != "" {
		return p
	}
	wd, _ := os.Getwd() // .../hetairoi/internal/sdkdriver
	return filepath.Clean(filepath.Join(wd, "..", "..", "..", "ahsir"))
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func bootFacade(t *testing.T, ahsirRepo string) string {
	t.Helper()
	work := t.TempDir()
	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	goEnv := append(os.Environ(), "GO111MODULE=on")
	for _, b := range []struct{ pkg, out string }{{"./cmd/ahsir", "ahsir"}, {"./cmd/ahsir-agent", "ahsir-agent"}} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, b.out), b.pkg)
		cmd.Dir = ahsirRepo
		cmd.Env = goEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", b.pkg, err, out)
		}
	}
	if out, err := exec.Command("codesign", "--force", "--sign", "-",
		filepath.Join(bin, "ahsir"), filepath.Join(bin, "ahsir-agent")).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v\n%s", err, out)
	}

	sched := freePort(t)
	facade := freePort(t)
	rangeStart := freePort(t)
	cfgPath := filepath.Join(work, "ahsir.yaml")
	cfg := fmt.Sprintf("agents: []\nregistry:\n  host: \"127.0.0.1\"\n  port: %d\n  heartbeat_interval: 10s\n  heartbeat_timeout: 30s\ntimeouts:\n  chat: 10m\n  task_status: 30s\nport_range:\n  start: %d\n  end: %d\n",
		sched, rangeStart, rangeStart+40)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	logf, _ := os.Create(filepath.Join(work, "ahsir.log"))
	cmd := exec.Command(filepath.Join(bin, "ahsir"), "start",
		"--cma-listen", fmt.Sprintf("127.0.0.1:%d", facade),
		"--cma-scheduler", fmt.Sprintf("http://127.0.0.1:%d", sched),
		"--cma-state", filepath.Join(work, "cma-state.json"),
		cfgPath)
	cmd.Dir = ahsirRepo
	cmd.Env = append(os.Environ(),
		"GO111MODULE=on", "CMA_RUNTIME_PROVIDER=echo", "AHSIR_ADMIN_TOKEN=it-tok",
		"HOME="+work, "no_proxy=127.0.0.1,localhost", "NO_PROXY=127.0.0.1,localhost")
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ahsir: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logf.Close()
	})

	url := fmt.Sprintf("http://127.0.0.1:%d", facade)
	waitHTTP(t, url+"/v1/agents", 90*time.Second, filepath.Join(work, "ahsir.log"))
	return url
}

func waitHTTP(t *testing.T, url string, timeout time.Duration, logPath string) {
	t.Helper()
	hc := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := hc.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	logs, _ := os.ReadFile(logPath)
	t.Fatalf("facade %s not ready in %s\n--- ahsir log ---\n%s", url, timeout, logs)
}
