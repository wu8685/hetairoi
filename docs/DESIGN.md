# hetairoi — Design

## Goal

Expose an **Anthropic Managed Agents (CMA) compatible HTTP API** so the official
`anthropic` SDK — pointed at this service's `base_url` — drives it as a drop-in.
Internally, every session is served by an [ahsir](https://github.com/wu8685/ahsir)
agent process.

```
official anthropic SDK ──/v1/agents, /v1/environments, /v1/sessions,
   (x-api-key,             /v1/sessions/{id}/events[/stream]──▶  hetairoi  ──▶ ahsir gateway
    anthropic-version,                                              │                (/admin/agents,
    anthropic-beta:                                                 │                 /agents/{name}/chat,
    managed-agents-2026-04-01)                                      │                 /a2a/{name}, /history)
                                                                    ├─ auth: x-api-key → ahsir token
                                                                    ├─ resources: agent_id/version/session_id ↔ ahsir name/contextId
                                                                    └─ translate: CMA shapes ⇄ ahsir card/chat/stream
```

hetairoi plays the role of Anthropic's managed-agents **control plane**; ahsir
is the orchestration layer **and** the tool-execution container (its `claude`/`codex`
subprocess runs the agent loop and executes bash/read/edit in the agent workspace).

## The four CMA resources and how they map

| CMA resource | Endpoint(s) | ahsir backing |
|---|---|---|
| **Agent** — persisted, versioned config (model/system/skills/mcp/tools) | `/v1/agents` | one ahsir agent per `(agent_id, version)`, named `cma-<id>-v<n>`; registered via `POST /admin/agents` with an inline card |
| **Environment** — container template | `/v1/environments` | logical only: a synthesized `env_id`. Isolation is ahsir's workspace + `filesystem` allow-list. No real container is provisioned. |
| **Session** — stateful run | `/v1/sessions` | `session_id → (ahsir agent name, contextId)` mapping held in the store |
| **Events** — send / stream / list | `/v1/sessions/{id}/events[/stream]` | send `user.message` → `POST /agents/{name}/chat`; stream = live event tail; list = event log |

**Versioning lives entirely in hetairoi.** ahsir sees a flat set of distinct
agents and is version-agnostic. Each agent update creates a new immutable version
snapshot in the store; a session pins the version at create time and resolves to
the matching `cma-<id>-v<n>` ahsir agent.

## Request flow (a turn)

1. `agents.create` → store version 1. `agents.update` → version N+1.
2. `environments.create` → synthesize `env_id`.
3. `sessions.create(agent, environment_id)`:
   - resolve agent (version, or latest),
   - `ensureRegistered`: translate the agent snapshot → ahsir card →
     `POST /admin/agents` (once per `cma-<id>-v<n>` per process),
   - allocate a `contextId`, store `session_id → (name, contextId)`,
   - return the session (`status: idle`).
4. `events.send(user.message)` → `runTurn` enqueues a turn on the session's serial
   executor (turns never interleave). `executeTurn`:
   - append `session.status_running`,
   - drive `POST /a2a/{name}` `message/stream` (SSE); accumulate text deltas and
     publish the A2A `taskId` to the record (so an interrupt can cancel this turn),
   - append one `agent.message` (the buffered reply) and `session.status_idle{end_turn}`.
   - `events.send(user.interrupt)` runs out of band: it reads the in-flight `taskId`
     and `POST /a2a/{name}` `tasks/cancel`; the running turn observes the cancel and
     settles back to idle.
5. `events.stream` (SSE) → subscribe **before** writing headers, then live-tail the
   event bus. No history replay (per CMA semantics — history is `events.list`).

## Package layout

| Package | Responsibility |
|---|---|
| `internal/cma` | CMA wire types (the external API surface) + id minting |
| `internal/ahsir` | client to the ahsir scheduler gateway + the inline `AgentCard` struct |
| `internal/translate` | CMA agent → ahsir card; `(agent_id, version)` → ahsir agent name |
| `internal/store` | resource store (agents/versions, environments, sessions) + per-session event log/bus; whole-file JSON persistence |
| `internal/config` | env-based config |
| `internal/api` | routing (`net/http` 1.22 ServeMux), auth, handlers, turn execution |
| `cmd/hetairoi` | entrypoint |
| `e2e` | official-SDK end-to-end tests + `fakeahsir` backend |

## Wire-shape alignment (important)

The external types in `internal/cma/types.go` are aligned to the **actual installed
official SDK** (`anthropic` 0.97.0), discovered by introspecting its pydantic models —
not guessed from docs. Lessons baked in (regressions to avoid):

- **Required list/map fields must serialize as `[]` / `{}`, never `null` or absent.**
  Agent `tools/skills/mcp_servers/metadata` are required → no `omitempty`, normalize
  nil → empty (`normalizeAgent`).
- **Agent needs `updated_at`**; **Environment needs `updated_at`** (+ required `metadata`).
- **`session.agent` is a FULL agent object** (`SessionAgent`: id, name, model, version,
  type, tools/skills/mcp_servers), not a bare ref. Session also requires `resources`,
  `stats`, `usage`, `vault_ids` (empty `[]`/`{}` validate).
- **List endpoints are cursor pages**: `{data, next_page}` (SDK `SyncPageCursor`), not
  `{data, has_more, first_id, last_id}`.
- **`events.send` returns `{}`** (`SendSessionEvents.data` is optional).
- **`session.error.error` is a discriminated union**: `{type:"unknown_error", message,
  retry_status:{type:"terminal"}}` — `type:"api_error"` is NOT a valid member.
- **Stream events** (each SSE `data:` JSON) are discriminated by `type`. The MVP emits
  `session.status_running`, `agent.message` (`content:[{type:"text",text}]`),
  `session.status_idle` (`stop_reason:{type:"end_turn"}`), `session.status_terminated`,
  `session.error`. The SDK validates each against its event union by `type`.

**When changing any response shape, re-introspect the SDK rather than guessing.** See
`ROADMAP.md` → "How to verify wire shapes".

## The ahsir backend contract

hetairoi depends on this ahsir surface (all but one already exist today):

| Need | ahsir endpoint | Status |
|---|---|---|
| register agent with inline config | `POST /admin/agents` **with a `card` body** | ✅ **landed** (scaffolds workspace + writes `.a2a/agent-card.yaml`) |
| stream a turn | `POST /a2a/{name}` `message/stream` (SSE) | ✅ — the live turn transport (`internal/ahsir/a2a.go`) |
| cancel a turn | `POST /a2a/{name}` `tasks/cancel` | ✅ — backs `user.interrupt` |
| send a turn (sync) | `POST /agents/{name}/chat` | ✅ (legacy; superseded by the stream path) |
| history | `GET /agents/{name}/history/{contextId}` | ✅ (client present; not yet wired to `events.list`) |
| delete / GC | `DELETE /admin/agents/{name}` | ✅ — refcount-aware session archive |

Inline registration has landed on the ahsir side (card json tags + `WriteCard` +
managed-workspace scaffolding). A full real end-to-end turn still needs a live LLM
provider (ahsir spawns a `claude`/`codex` subprocess). **Any ahsir-side change must be
discussed and confirmed with the ahsir maintainer before implementation** — ahsir stays a pure agent
runtime.

## ahsir streaming reality (verified against ahsir code)

ahsir's A2A `message/stream` only surfaces **incremental text deltas** (with
`streaming.partial_messages: true`, as `status-update` events) and a **terminal
Task**. It does NOT put `thinking`, `tool_use`, or sub-agent-call events on the wire
(they're consumed internally). So:

- `runTurn` consumes that stream (`internal/ahsir/a2a.go`) and **buffers the deltas
  into a single `agent.message`**. We do not emit per-delta CMA events because the
  CMA SDK `agent.message` carries a *complete* message (no delta/partial field) and
  there is no `agent.message_delta` type — token-incremental CMA streaming isn't
  expressible until the CMA event model gains a delta event.
- `agent.thinking` / `agent.tool_use` in CMA sessions, and multiagent `thread_*`,
  require ahsir to surface those events — deferred, and ahsir-side work.
