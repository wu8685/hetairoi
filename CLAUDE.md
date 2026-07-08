# hetairoi — agent orientation

This project is a **gateway** that exposes an **Anthropic Managed Agents (CMA)
compatible HTTP API**, backed internally by an [ahsir](https://github.com/wu8685/ahsir)
agent fleet. The official `anthropic` SDK, pointed at this service's `base_url`,
drives it as a drop-in.

**Read first:** `docs/DESIGN.md` (architecture, concept mapping, wire-shape rules,
ahsir contract) and `docs/ROADMAP.md` (status, phases, the pending ahsir change,
how to verify wire shapes). `README.md` is the short version.

## Repo map

```
cmd/hetairoi/      entrypoint
internal/cma/         CMA wire types (the external API surface) + ids
internal/ahsir/       ahsir gateway client + inline AgentCard
internal/translate/   CMA agent → ahsir card; (agent_id,version) → ahsir name
internal/store/       resources + session↔(name,contextId) + event bus (JSON persist)
internal/api/         routing + auth + handlers + turn execution
e2e/                  official-SDK end-to-end tests + fakeahsir backend
```

## Build / test (this machine)

- Module is inside `GOPATH/src` with `GO111MODULE=off` global → **prefix with `GO111MODULE=on`**.
- This Mac **SIGKILLs directly-run freshly-built binaries** → use **`go run`**, not a built binary.

```sh
GO111MODULE=on go build ./...        # compile check
GO111MODULE=on go vet ./...
GO111MODULE=on go run ./cmd/hetairoi   # run the server (not a built binary)
python3 -m pip install -r e2e/requirements.txt
./e2e/run.sh                          # official-SDK e2e (boots fakeahsir + hetairoi via go run)
```

## Constraints that matter

1. **Keep response shapes aligned to the installed `anthropic` SDK.** Do not guess —
   introspect the SDK's pydantic models before changing any response (recipe in
   `docs/ROADMAP.md` → "How to verify wire shapes"). The required-field gotchas are in
   `docs/DESIGN.md` → "Wire-shape alignment".
2. **Versioning lives here, not in ahsir.** Each `(agent_id, version)` → a distinct
   ahsir agent `cma-<id>-v<n>`. ahsir is version-agnostic.
3. **Any ahsir-side change is discussed with the ahsir maintainer first.** ahsir stays a pure agent
   runtime. The one pending ahsir change (inline agent registration) is specced in
   `docs/ROADMAP.md`; hetairoi already speaks that contract.
4. **`e2e/` must stay green.** It's the contract check against the real SDK.

## Current status

P1 + P2 done and e2e-green (9/9), `go test ./...` green under `-race`. On top of the
MVP: turns run over the A2A `message/stream` transport (deltas buffered into one
`agent.message`, since the CMA SDK `agent.message` has no delta field); per-session
FIFO turn serialization; immediate event persistence; `events.list` cursor pagination;
`user.interrupt` → A2A `tasks/cancel`; refcount-aware session archive GC. The ahsir
inline-registration change **landed** in `../ahsir` (card json tags + `WriteCard` +
managed-workspace scaffolding) — a full real turn still needs a live LLM provider.
Next is P3 (`agent.thinking`/`agent.tool_use`, multiagent, outcomes, custom tools).
See `docs/ROADMAP.md`.
