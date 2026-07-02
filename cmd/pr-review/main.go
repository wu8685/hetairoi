// Command pr-review runs cma-service with a built-in CodeHub PR poller
// wired to an ahsir-backed code-review agent.
//
// Flow:
//
//	CodeHubPRSource (poll every CMA_CODEHUB_INTERVAL)
//	  codehub pr list --reviewer @me --state opened  -> one event per PR
//	  Event.ID = pr-<iid>-<head_sha>  (unchanged PR deduped; fix push re-triggers)
//	    -> subscription "pr-review" (Keyed by iid: one session per PR)
//	      -> pr-reviewer agent (Claude runtime, shell + codehub tools)
//	          clone/fetch -> review diff -> comment (issues) OR approve (clean)
//
// Scope guard: CMA_CODEHUB_ALLOW_IIDS limits which PRs are processed (the first
// run should pin a single iid before opening up to all reviewer=@me PRs).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wu8685/cma-service/internal/ahsir"
	"github.com/wu8685/cma-service/internal/api"
	"github.com/wu8685/cma-service/internal/cma"
	"github.com/wu8685/cma-service/internal/config"
	"github.com/wu8685/cma-service/internal/eventbus"
	"github.com/wu8685/cma-service/internal/store"
)

const reviewSystemPrompt = `You are a senior Go / Kubernetes code reviewer for the CodeHub project ` +
	"`example-org/k8s-extension`" + `. You drive the whole review yourself with the
` + "`codehub`" + ` CLI (v1.x, already authenticated as the operator) via your Bash tool. Do NOT
git clone — codehub reads the PR remotely. Run ` + "`codehub <cmd> --help`" + ` if unsure of flags.

For each pull request you are given (project, iid, source_branch, target_branch, head_sha):

1. READ the change (no clone):
   - Diff:            codehub pr diff <iid> -P <project> --no-pager
   - File at PR head: codehub cat <source_branch>:<path> -P <project> --no-pager  (for fuller context)
2. REVIEW for real defects: correctness, concurrency/goroutine leaks, nil/err handling,
   resource cleanup, context cancellation, API/CRD compatibility, security. Be specific; cite file:line.
3. CHECK what you already said (this may be a re-review after a fix push):
   - codehub review comments list <iid> -P <project> --json
   - Never repeat a comment you already made; if a prior issue is now fixed, acknowledge and move on.
4. DECIDE and ACT (these are REAL writes to a shared repo — be deliberate):
   - If you find issues: post each as a line-level comment, then STOP. Leave the PR open:
       codehub review comments add <iid> -P <project> --type Problem \
         --file <path> --line <new_line_no> -m "<concrete, actionable finding in Chinese>"
     Use --type Comment (no --file/--line) for an overall note. Do NOT approve when you posted Problems.
   - If the diff is clean / all prior issues resolved: approve it, then post a brief summary comment:
       codehub pr approve <iid> -P <project>
       codehub review comments add <iid> -P <project> --type Comment -m "<what you checked, in Chinese>"

Rules: run real commands and use real output — never fabricate file contents, line numbers, or results.
Keep the review bounded and decisive (don't loop). End your reply with a 3-5 line Chinese summary:
what you reviewed, the verdict (commented / approved), and the exact codehub write commands you ran.`

func main() {
	cfg := config.Load()

	st, err := store.New(cfg.StateFile)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	ac := ahsir.New(cfg.AhsirURL, cfg.AhsirAdminToken)
	srv := api.New(cfg, st, ac)

	model := env("CMA_REVIEW_MODEL", "") // empty -> claude CLI default (inherits OAuth)
	agentID := seedAgent(st, "pr-reviewer", model, reviewSystemPrompt)
	envID := seedEnv(st, "pr-review")
	log.Printf("seeded agent=%s env=%s model=%q", agentID, envID, model)

	project := env("CMA_CODEHUB_PROJECT", "example-org/k8s-extension")
	reviewer := env("CMA_CODEHUB_REVIEWER", "@me")
	interval := durEnv("CMA_CODEHUB_INTERVAL", 30*time.Second)
	allow := parseIIDs(os.Getenv("CMA_CODEHUB_ALLOW_IIDS"))

	bus := eventbus.New(srv.BusDriver(), filepath.Dir(cfg.StateFile), 8)
	err = bus.Register(eventbus.Subscription{
		Name:  "pr-review",
		Match: func(e eventbus.Event) bool { return e.Type == "pr" },
		// Keyed by PR iid: one session per PR, so a fix-push re-review reuses the
		// same conversation (the agent sees its earlier findings).
		Policy: eventbus.Keyed{
			Agent:  eventbus.AgentRef{ID: agentID},
			EnvID:  envID,
			Key:    func(e eventbus.Event) string { return e.Subject },
			Prompt: reviewPrompt,
		},
	})
	if err != nil {
		log.Fatalf("register handler: %v", err)
	}
	srv.SetEventBus(bus)

	src := eventbus.CodeHubPRSource{
		Bin:       env("CMA_CODEHUB_BIN", "codehub"),
		Project:   project,
		Reviewer:  reviewer,
		State:     "opened",
		AllowIIDs: allow,
	}
	poller := eventbus.NewPoller("codehub-pr", interval, bus, src.Fetch).
		OnResult(func(rs []eventbus.DispatchResult) {
			for _, r := range rs {
				if r.Skipped {
					continue // unchanged PR — deduped, no work
				}
				log.Printf("dispatch pr iid=%s session=%s err=%v", r.EventID, r.SessionID, r.Err)
			}
		})
	go func() {
		if err := poller.Run(context.Background()); err != nil && err != context.Canceled {
			log.Printf("poller stopped: %v", err)
		}
	}()

	log.Printf("pr-review: polling %s reviewer=%s every %s allow=%v on %s",
		project, reviewer, interval, keys(allow), cfg.Listen)
	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// reviewPrompt shapes one PR event into the per-turn instruction for the agent.
func reviewPrompt(e eventbus.Event) string {
	return fmt.Sprintf("Review this pull request now.\n\n%s\n\n"+
		"Follow your standing review procedure exactly.", string(e.Payload))
}

func seedAgent(st *store.Store, name, model, system string) string {
	now := time.Now().UTC()
	a := &cma.Agent{
		Type: "agent", ID: cma.NewID(cma.PrefixAgent), Version: 1, Name: name,
		Model: cma.ModelConfig{ID: model}, System: system,
		Tools: []cma.ToolDef{}, Skills: []cma.SkillRef{}, MCPServers: []cma.MCPServer{},
		// shell_access opts this agent into the Bash tool (ahsir
		// filesystem.shell_access) so it can run codehub itself; runtime_timeout
		// widens ahsir's 120s cap for the multi-command review turn.
		Metadata: map[string]string{"shell_access": "true", "runtime_timeout": "900s"},
		CreatedAt: now, UpdatedAt: now,
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

func parseIIDs(s string) map[int]bool {
	out := map[int]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				out[n] = true
			}
		}
	}
	return out
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func durEnv(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
