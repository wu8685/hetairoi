# Event Bus — dynamic control plane (CodeHub PR review)

Same outcome as [`../pr-review`](../pr-review) — an CodeHub PR gets
reviewed by an ahsir agent that comments / approves over the `codehub` CLI — but
**nothing is wired in Go**. The agent, the event source, and the handler are all
created at runtime over HTTP against the stock `cmd/cma-service`:

```
cmd/cma-service (mounts the eventbus registry)
   POST /v1/agents                  → create the review agent (shell_access)
   POST /v1/eventbus/sources        → codehub-pr poller for the repo
   POST /v1/eventbus/handlers       → keyed handler: one session per PR
        │
        └─ poller (every 30s) → event pr-<iid>-<head_sha>
             → handler "pr-review" (Keyed by iid)
               → review agent: codehub pr diff → review → comment / approve
```

This is the **Phase-2** path: add a new repo/handler to a long-running
cma-service with a request, not a redeploy. Specs are persisted to
`<state-dir>/eventbus/_registry.json` and rebuilt on the next boot.

## Run

```sh
./run.sh                 # review PR 3177 (default)
REVIEW_IID=3179 ./run.sh   # a different PR
```

It boots an isolated ahsir (`19800`, with its web UI on `19801`) + the real
`cma-service` (`18790`), then drives the whole chain over HTTP and streams the
agent's `tool_use` / comments / approve. Watch live at <http://127.0.0.1:19801>.

⚠️ These are **real writes** to a shared repo (a comment and an `codehub pr
approve`) under your CodeHub identity, scoped to the single `REVIEW_IID`.

## The three API calls

```sh
# 1. the review agent — metadata.shell_access grants it the Bash tool
curl -X POST :18790/v1/agents -d '{"name":"pr-reviewer","model":{"id":"claude-sonnet-4-6"},
  "system":"<review procedure>","metadata":{"shell_access":"true","runtime_timeout":"900s"}}'

# 2. the source — poll the repo for PRs where I'm a reviewer
curl -X POST :18790/v1/eventbus/sources -d '{"name":"pr-source","type":"codehub-pr",
  "project":"example-org/k8s-extension","reviewer":"@me","interval":"30s",
  "allow_iids":[3177]}'

# 3. the handler — one session per PR (keyed by iid), prompt from the event
curl -X POST :18790/v1/eventbus/handlers -d '{"name":"pr-review","match":{"type":"pr"},
  "policy":{"kind":"keyed","agent_id":"agent_…","env_id":"env_…",
    "key_template":"{{.subject}}",
    "prompt_template":"Review PR {{.payload.iid}} now …"}}'
```

`GET /v1/eventbus/{sources,handlers}` lists them; `DELETE …/{name}` removes one.

See [`docs/EVENTBUS-SOURCES.md`](../../docs/EVENTBUS-SOURCES.md) for the full
schema and the `shell_access` requirement.
