# hetairoi

An **Anthropic Managed Agents (CMA) compatible API**, backed by an
[ahsir](https://github.com/wu8685/ahsir) agent fleet.

Point the official `anthropic` SDK at this service's `base_url` and drive it as a
drop-in: create agents, environments, sessions, send events, stream the agent's
output. Internally each session is served by an ahsir agent process.

```
official anthropic SDK ──/v1/agents, /v1/environments, /v1/sessions,
                          /v1/sessions/{id}/events[/stream]──▶ hetairoi ──▶ ahsir gateway
```

## What it maps

| CMA concept | ahsir backing |
|---|---|
| Agent (versioned config: model/system/skills/mcp) | an ahsir agent (`agent-card.yaml`); each `(agent_id, version)` → a distinct ahsir agent `cma-<id>-v<n>` |
| Environment | logical resource (synthesized `env_id`); isolation via ahsir's workspace + `filesystem` allow-list |
| Session | `session_id → (ahsir agent name, contextId)` |
| `events.send(user.message)` | `POST /agents/{name}/chat` |
| `events.stream` (SSE) | live tail; MVP: one `agent.message` + `session.status_idle` per turn |
| `events.list` | event log (history replay) |

Versioning lives entirely here — ahsir stays version-agnostic.

## Status (P1 + P2)

Implemented and exercised end-to-end by the official SDK (`e2e/`, 9/9 green;
`go test ./...` green under `-race`): agents (create / retrieve / version / list),
environments, sessions (create / retrieve / archive / list), `user.message`,
`user.interrupt`, live event stream (`session.status_running` → `agent.message` →
`session.status_idle`), event list with cursor pagination.

Turns run over the A2A `message/stream` transport (deltas buffered into one
`agent.message`), strictly serialized per session, with events persisted on append.
`user.interrupt` cancels a live turn via A2A `tasks/cancel`. Session archive reclaims
the backing ahsir agent only when no other live session pins that `(agent_id,
version)`.

**Deferred:** custom tools, `user.tool_confirmation`, multiagent, outcomes, webhooks,
vaults/MCP auth; `agent.thinking` / `agent.tool_use` events (need ahsir to surface
those on its A2A wire). Token-incremental CMA streaming is not expressible in the
current SDK (`agent.message` is a complete message, no delta event) — see
`docs/ROADMAP.md`.

## Validated end to end against real ahsir + a real LLM

The full stack was run against a **real ahsir scheduler + DeepSeek** (2026-06-14):
`sessions.create` → inline ahsir registration (`POST /admin/agents` with an inline
`card`) → real `ahsir-agent` spawn → real streaming reply → multi-turn context
continuity → `user.interrupt` aborting the in-flight turn within ~1s. The ahsir-side
changes (inline registration + streaming cancel) are in `../ahsir`; see
`docs/ROADMAP.md` → "Real-run validation".

## Run

```sh
# needs Go 1.23+
CMA_LISTEN=127.0.0.1:8787 \
CMA_AHSIR_URL=http://127.0.0.1:9800 \
CMA_AHSIR_ADMIN_TOKEN=... \
go run ./cmd/hetairoi
```

Config (env): `CMA_LISTEN`, `CMA_AHSIR_URL`, `CMA_AHSIR_ADMIN_TOKEN`,
`CMA_API_KEYS` (comma-separated; empty = open), `CMA_STATE_FILE`,
`CMA_RUNTIME_PROVIDER` / `CMA_RUNTIME_BASE_URL` / `CMA_RUNTIME_API_KEY`
(provider credentials baked into every ahsir agent card).

## Tests

```sh
go test ./...        # unit (when added)
./e2e/run.sh         # official-SDK end-to-end (see e2e/README.md)
```
