# hetairoi — Status & Roadmap

## Current status (2026-06-14)

**P1 + P2 built and verified end-to-end by the official `anthropic` SDK.** `e2e/`
is 9/9 green, driving hetairoi with the real SDK against a fake-ahsir backend
(now over the A2A `message/stream` transport). `go test ./...` is green with the
race detector across `internal/{cma,store,translate,ahsir,api}`.

Implemented:

- **Agents**: create / retrieve (with `?version=`) / update (→ new version) / list.
- **Environments**: create / retrieve / **update / archive / delete** / list (logical resource).
- **Sessions**: create (resolves agent version, registers the ahsir agent, allocates
  contextId) / retrieve / **update** / archive / **delete** / list. Archive and delete
  are **refcount-aware**: they reclaim the backing ahsir agent (`DELETE /admin/agents`)
  only when no other live session pins the same `(agent_id, version)`. Delete cancels any
  in-flight turn and emits `session.deleted` to live subscribers first.
- **Agents**: archive now sets `archived_at` (was a no-op marker).
- **Events**: `send(user.message)`, `user.interrupt`, live SSE `stream`, `list`
  (with `limit` / `order` / `page` cursor pagination). Outbound now also includes
  `session.deleted` (on delete) and `session.status_rescheduled`.
- **List endpoints** (agents / environments / sessions): deterministic order
  (created_at, then id — the store's map iteration is random), `include_archived`
  (default **excludes** archived), `limit` / `page` cursor pagination, and
  `sessions.list` `agent_id` / `agent_version` / `order` filters.

### Resource lifecycle semantics

- **Archive is a real lifecycle, not just a marker**: archived agents / environments
  are excluded from `list` by default and are **rejected as inputs to
  `sessions.create`** (`agent is archived` / `environment is archived`). Agent archive
  marks **every version**. No unarchive yet.
- **session.delete** cancels any in-flight turn, waits (bounded) for it to settle so
  the ahsir agent isn't GC'd out from under a live stream, emits `session.deleted` to
  live subscribers, then removes the session and refcount-GCs the agent.
- **environment.delete** is unconditional: an environment is only dereferenced at
  `sessions.create` (it synthesizes the session's `env_id`); existing sessions never
  re-read it, so deleting an in-use environment does not affect running sessions. No
  cascade / 409 is needed.
- **session.status_rescheduled** is emitted when a turn's connect to its agent had to
  retry because the agent wasn't reachable yet — covering both the initial bind right
  after registration and a supervised restart mid-session. It's an approximation of
  CMA's "compute is being (re)scheduled" signal (we don't distinguish initial-bind from
  restart); validated deterministically by `TestExecuteTurn_EmitsRescheduled`.
- **Turn execution**: driven over A2A `message/stream`; deltas accumulated into one
  `session.status_running` → `agent.message` → `session.status_idle{end_turn}`. The
  A2A `taskId` is captured so `user.interrupt` → `tasks/cancel` can stop a live turn.
- **Concurrency**: a session's turns run strictly FIFO (per-session serial executor) —
  no interleaved event streams.
- **Durability**: events persist immediately on append (crash-safe mid-turn); the
  whole-file JSON save snapshots event logs under lock (no encoder/append race).
- Auth (`x-api-key`, open if no keys).

Deferred (not yet built) — by SDK surface:

- **whole resources not yet routed**: `files` (upload/download/list/delete),
  `skills` (custom skill CRUD), `vaults` (secret store for MCP/tool auth),
  `sessions.resources` (attach files/data to a session)
- custom tools (`agent.custom_tool_use` / `user.custom_tool_result`)
- `user.tool_confirmation`
- outcomes / grader, multiagent (coordinator / threads), webhooks, memory stores
- observability events that need ahsir to surface them on the A2A wire:
  `agent.thinking`, `agent.tool_use` / `agent.tool_result`, `agent.mcp_tool_*`,
  `span.model_request_*`
- token-incremental CMA streaming: **not expressible in the current SDK** — its
  `agent.message` event carries a *complete* message (no delta/partial field) and
  there is no `agent.message_delta` type. We therefore buffer A2A deltas into one
  `agent.message`. Revisit if/when the CMA event model gains a delta event.

## Phases

| Phase | Scope | State |
|---|---|---|
| **P1 (MVP)** | agents/versions, environments, sessions, `user.message`, live stream, event list | ✅ done, e2e green |
| **P2** | A2A `message/stream` turn transport; `user.interrupt` → `tasks/cancel`; `events.list` cursor pagination; turn serialization; event durability; refcount GC; **ahsir inline registration (landed)** | ✅ done, e2e green |
| **P3** | `agent.thinking` / `agent.tool_use` events (ahsir support); multiagent via ahsir rooms / A2A; outcomes; webhooks; vaults/MCP auth; custom tools | next |

## Real-run validation (2026-06-14, DeepSeek provider)

The full stack was exercised end to end against a **real ahsir scheduler + real LLM**
(DeepSeek via the Anthropic-compatible endpoint, `CMA_RUNTIME_PROVIDER=deepseek`):

- `sessions.create` → inline registration → ahsir scaffolds the card and spawns a real
  `ahsir-agent` (claude CLI → DeepSeek). ✅
- `user.message` → A2A `message/stream` → **real DeepSeek reply**, `stop_reason: end_turn`. ✅
- multi-turn **context continuity** (turn 1 states a fact, turn 2 recalls it). ✅
- `user.interrupt` → A2A `tasks/cancel` → **the in-flight turn aborts within ~1s**
  (claude subprocess killed) and the session settles back to `idle`. ✅

The first-turn-after-registration race (the agent's A2A server binds its port ~1s after
the admin start returns) is handled by a bounded connect retry in
`internal/ahsir/a2a.go` (`openStream`); it also covers the brief port gap during ahsir's
supervised agent restarts.

### Two ahsir-side gaps fixed to make interrupt effective

Real-run testing surfaced that the cma-side interrupt was correct but ahsir didn't act on
it for streaming turns. Two ahsir fixes (in `../ahsir`, confirmed with the ahsir maintainer):

1. **`OnCancelTask` reordered** (`internal/wrapper/server.go`): a streaming turn's task
   isn't persisted to the task store until it completes, so the old `tasks.Get`-first
   check returned "not found" and never reached the cancel. It now fires the registered
   cancel *before* the store lookup.
2. **ClaudeSession honors ctx cancellation** (`internal/wrapper/session_claude.go`):
   turns were only abortable by the timeout timer, never by ctx. `Stream` now watches
   `ctx.Done()` and kills the subprocess (same EVICTED + `--resume` recovery as a
   timeout); `OnSendMessageStream` registers the streaming turn's cancel in
   `asyncCancels` by task id; the executor maps the new `ErrTurnCanceled` →
   `TaskStateCanceled` so a deliberate interrupt settles as canceled, not failed.
   (CodexSession already uses a per-turn `context.WithTimeout`; ctx-cancel parity there
   is a follow-up.)

## ahsir inline registration — LANDED (2026-06-14)

`sessions.create` registers a versioned ahsir agent via `POST /admin/agents` with an
**inline `card`** (one ahsir agent per `(agent_id, version)`). The required ahsir-side
change is implemented (confirmed with the ahsir maintainer):

- `internal/wrapper/card.go`: `AgentCardConfig` and its nested structs now carry `json`
  tags equal to their `yaml` keys; new `WriteCard(workspaceDir, cfg)` persists a decoded
  inline card as `.a2a/agent-card.yaml`.
- `internal/scheduler/config.go`: `ManagedAgentWorkspace(name)` → `.ahsir/agents/<name>`.
- `internal/scheduler/gateway.go`: `startAgentRequest` gained `Card *wrapper.AgentCardConfig`;
  `handleAdminStart` scaffolds the workspace + writes the card before `StartAgent`,
  allocating a managed workspace when `workspace` is empty.

hetairoi speaks this contract in `internal/ahsir/{card.go,client.go}`. **Still needs a
live run with a real LLM provider** to exercise a real turn end to end (ahsir spawns a
`claude`/`codex` subprocess); the CMA side and the wire contract are unit- and
SDK-verified now.

> **Rule:** any ahsir change is discussed and confirmed with the ahsir maintainer first. ahsir stays a
> pure agent runtime; versioning and CMA semantics live here.

## Open decisions / notes

- **Streaming**: `runTurn` consumes ahsir's A2A `message/stream` SSE (`POST /a2a/{name}`,
  `partial_messages` set on the card). The minimal A2A client lives in
  `internal/ahsir/a2a.go` (hand-rolled to keep hetairoi stdlib-only; wire shapes
  verified against `a2a-go` v0.3.15). Deltas are buffered into one `agent.message` — see
  the "not expressible" note above for why we don't emit per-delta events.
- **Persistence**: whole-file JSON in `internal/store` is still MVP-grade; swap for a
  real store before scale. Event durability and the save-race are fixed; the remaining
  cost is full-file rewrite per event.
- **Model id**: the SDK response model accepts any string; `claude-opus-4-8` is fine.

## How to verify wire shapes (don't guess)

The installed SDK is the source of truth. Introspect it before changing any response:

```python
import anthropic, inspect
c = anthropic.Anthropic(api_key="x", base_url="http://localhost:1")
print(inspect.signature(c.beta.sessions.create))
# required fields of a response model:
import anthropic.types.beta as tb
cls = tb.BetaManagedAgentsSession
for k, v in cls.model_fields.items():
    print(k, "required=", v.is_required(), v.annotation)
```

Event models live under `anthropic/types/beta/sessions/beta_managed_agents_*_event.py`.

## Local dev notes (this machine)

- The module is inside `GOPATH/src` with `GO111MODULE=off` globally → prefix go
  commands with `GO111MODULE=on`.
- This corporate Mac **SIGKILLs directly-executed freshly-built binaries** (rc=137).
  Run via `go run ./cmd/hetairoi` (and `go run ./e2e/fakeahsir`), not a built
  binary. `go build`/`go vet` work fine for checking.
- git.internal.example.com is unreachable from some sandboxes (SSH:22 and HTTPS:443 time out);
  push from an environment with the corporate VPN.
