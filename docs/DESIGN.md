# Hetairoi — Design

## Goal

Turn **events in the world** into **agent work** on an
[ahsir](https://github.com/wu8685/ahsir) fleet. Hetairoi watches sources, matches
events against declarative handlers, and drives ahsir agents to act — without any
bespoke glue per use case. It is the *scenario / orchestration* layer; ahsir is the
*runtime* that actually schedules agents and runs their tool loops.

## Two processes, one seam

```
sources (GitHub / CodeHub / workitems / webhook)
        │  events
        ▼
Hetairoi  ── eventbus: match → policy → session/turn ──┐
  :18791                                               │  eventbus.SessionDriver
                                                        │  (official anthropic-sdk-go)
                                                        ▼
ahsir  ── CMA facade :18790 ──in-proc──▶ scheduler :9800 ──▶ agent (claude/codex) ──▶ does the work
```

Hetairoi and ahsir are **separate processes**. Hetairoi is a **CMA client**: it
creates agents/environments/sessions and runs turns on ahsir's CMA facade through
the official SDK. All agent state — cards, sessions, contexts, transcripts — lives
on ahsir. Hetairoi holds only its eventbus wiring (sources, handlers) and per-handler
dedup/persistence.

### Why this split

- **Separation of concerns.** ahsir is a general agent runtime; the "what should
  trigger which agent, and when" logic is orthogonal and changes far more often.
  Keeping it out of ahsir keeps the runtime clean and lets scenarios evolve
  independently.
- **Dogfooding the CMA API.** Hetairoi drives ahsir through the *same* official SDK
  an external integrator would use. If Hetairoi's own dev loop works, the CMA surface
  is provably complete and correct — the client and the contract test are the same code.
- **A client, not a fork.** Because it only speaks the CMA API, Hetairoi is decoupled
  from ahsir internals and could, in principle, drive any CMA-compatible runtime.

(Historically the CMA gateway lived *in* this repo; it was migrated into ahsir. The
full narrative is `docs/RFC-001-cma-gateway-into-ahsir.md`.)

## The eventbus (the core)

The eventbus is `internal/eventbus`. Full spec: **[EVENTBUS-SPEC.md](EVENTBUS-SPEC.md)**
(policies, dispatch, dedup, concurrency) and **[EVENTBUS-SOURCES.md](EVENTBUS-SOURCES.md)**
(built-in sources + the dynamic control plane). In brief:

- **Sources** produce events — poll (GitHub / CodeHub / workitem) or the inbound
  webhook (`POST /eventbus/events`). Poll sources watch from `now` on (re)start and
  dedup by event id, so a restart never replays history. Multiple sources of the same
  kind can run at once — e.g. one GitHub source per watched repo — each polling
  independently.
- **Handlers** are declarative: a `match` (event type + payload predicates) → a
  `policy`. No closures, no redeploy — handlers/sources are created and torn down at
  runtime over `/v1/eventbus/{sources,handlers}`.
- **Policies** decide the session:
  - *stateless* — a fresh one-shot session per event;
  - *keyed* — one durable session per key (e.g. one session per issue/PR), so a
    follow-up event continues the same conversation;
  - *routed* — a router agent decides whether to reuse an existing session.
- **Dispatch** creates/reuses a session and runs the turn via the `SessionDriver`.
  Events are deduped by id and persisted per handler, so a crash mid-loop resumes
  cleanly. Turns for one session are strictly serialized.
- **Closed loop.** Agents can act with `shell_access` (post to GitHub, push commits),
  and an `approved`/human-merge gate lets the loop hand back to a person.

## The SessionDriver seam

`eventbus.SessionDriver` is the one interface between the eventbus and the runtime:

```go
type SessionDriver interface {
    CreateSession(agent AgentRef, envID string) (sessionID string, err error)
    SendUserMessage(sessionID, prompt string) error
    RunForReply(agent AgentRef, envID, prompt string) (reply string, err error) // one-shot
    SessionSummary(sessionID string) (SessionSummary, error)
}
```

The eventbus is runtime-agnostic — it only knows this seam. `internal/sdkdriver` is
the sole implementation, built on the official `anthropic-sdk-go`:

| SessionDriver method | anthropic-sdk-go call |
|---|---|
| `CreateSession` | `client.Beta.Sessions.New(agent, environment_id)` |
| `SendUserMessage` | `client.Beta.Sessions.Events.Send(sessionID, user.message)` |
| `RunForReply` | throwaway session → `Events.Send` → poll `Events.List` to idle → delete |
| `SessionSummary` | `client.Beta.Sessions.Get` + `Events.List` (first user / last agent message) |

`RunForReply` polls the persisted event log rather than the live SSE stream on
purpose: it's deterministic and immune to the subscribe/first-token race that an
instant reply can lose.

## Package layout

| Package | Responsibility |
|---|---|
| `internal/eventbus` | sources, handlers/policies, the bus, the runtime registry of handlers/sources, dedup + JSON persistence, the inbound webhook |
| `internal/sdkdriver` | `SessionDriver` on the official `anthropic-sdk-go`; drives ahsir's CMA facade |
| `internal/api` | slim HTTP surface: the eventbus control plane (`/v1/eventbus/{sources,handlers}`), the webhook (`/eventbus/events`), and `x-api-key` auth |
| `internal/config` | env config: `CMA_LISTEN`, `CMA_FACADE_URL`, `CMA_API_KEY`, `CMA_STATE_FILE` |
| `cmd/hetairoi` | entrypoint: build the SDK driver + eventbus, serve the control plane |

## Validation

The SDK path is checked by `internal/sdkdriver/driver_integration_test.go`: it builds
a real `ahsir` + `ahsir-agent`, boots `ahsir start --cma-listen` with a deterministic
`echo` provider, creates an agent/environment via the SDK, and exercises all four
`SessionDriver` methods end to end. Beyond that, the deployed dev loop is the live
system test — it runs real coder/reviewer agents through this exact path.
