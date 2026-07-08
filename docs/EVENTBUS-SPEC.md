# Event Bus — Spec

> Status: **draft for review** · Target: `internal/eventbus` (in-process package of hetairoi)
> Development: **TDD** — the acceptance scenarios in §12 are the test list.

## 1. Purpose

Turn inbound **events** (webhooks today) into agent **turns**. A user registers
**handlers** that subscribe to event types; when a matching event arrives, the
handler resolves a session (create or reuse), shapes the event into a prompt,
and drives that session for one turn. The agent does the work; the event source
does not see the reply (fire-and-forget). The resolved session stays addressable
so a human can pick it up and keep interacting via the normal CMA API.

Scenarios: **ops-alert auto-remediation** (group related alerts into one incident
session), **message-driven assistant** (one session per thread), and
**multi-agent collaboration** (agents emit events that trigger other agents —
deferred to after custom tools, see §11).

## 2. Boundary

- A **Go package** inside hetairoi with its own external interface (webhook).
- In-process: it drives sessions through a **SessionDriver** interface that
  hetairoi implements over its existing session/turn machinery — no HTTP
  self-loop. The bus depends only on the interface (testable against a fake).

```go
type SessionDriver interface {
    CreateSession(agent AgentRef, envID string) (sessionID string, err error)
    SendUserMessage(sessionID, prompt string) error          // drives one turn (async)
    RunForReply(agent AgentRef, envID, prompt string) (string, error) // one-shot turn → final text (for the router)
    SessionSummary(sessionID string) (SessionSummary, error) // derived from the event log
}

type SessionSummary struct {
    SessionID    string
    CreatedAt    time.Time
    LastActiveAt time.Time
    Seed         string // first user.message, truncated — the session's topic seed
    Last         string // most recent agent.message, truncated — current state
    Archived     bool
}
```

## 3. Data model

```go
type Event struct {
    ID      string          // dedup key (required)
    Type    string          // matched against subscriptions
    Subject string          // routing key (e.g. incident-id / thread-id / user-id)
    Payload json.RawMessage // raw event body
    Source  string          // origin (audit)
    Time    time.Time
    Hop     int             // loop guard — agent-emitted events increment this
    CauseID string          // id of the event that caused this one (causal chain)
}

type Subscription struct {
    Name   string            // identity; also the dedup-store namespace
    Match  func(Event) bool  // which events this handler subscribes to
    Policy Policy            // one of Stateless | Keyed | Routed
    Dedup  DedupConfig       // per-handler rotate window (§9)
}
```

## 4. Policies

Three tiers, by cost of the "which session?" decision:

### 4.1 Stateless — new session every event
```go
type Stateless struct {
    Agent  AgentRef
    EnvID  string
    Prompt func(Event) string
}
```
Every event creates a fresh session and sends `Prompt(event)`. No memory across
events.

### 4.2 Keyed — deterministic reuse, no LLM
```go
type Keyed struct {
    Agent  AgentRef
    EnvID  string
    Key    func(Event) string  // events with the same Key share one session
    Prompt func(Event) string
}
```
`Key(event)` → look up the bound session for `(subscription, key)`; reuse if
present (and not archived), else create and bind. Then send `Prompt(event)`.
Cheap, deterministic — the default for message-assistant (`Key = thread-id`).

### 4.3 Routed — an LLM router decides reuse
```go
type Routed struct {
    Agent  AgentRef        // handling agent (does the work)
    EnvID  string
    Router RouterSpec      // routing agent (decides)
    Prompt func(Event) string // fallback prompt if the router yields none (optional)
}

type RouterSpec struct {
    Agent        AgentRef   // routing agent (a cheap model)
    SystemPrompt string
    MaxCandidates int       // recency cap on candidates (default 20)
}
```
On event: build the candidate list (§6), run the router as a **one-shot
structured call** (§5), apply its decision, send the prompt to the chosen
session. For ops-alert grouping ("is this alert part of an existing incident?").

## 5. Routed decision (the router call)

The router is a **stateless** turn: `RunForReply(Router.Agent, env, routerPrompt)`.
The router prompt embeds the incoming event + the candidate summaries and asks
for a strict JSON reply:

```json
{ "reuse_session_id": "<id or empty>", "prompt": "<prompt for the handling agent>" }
```

The bus parses the reply:
- `reuse_session_id` non-empty and valid (exists, not archived) → reuse it.
- empty / unparseable / unknown / archived → **degrade to create a new session**
  (never crash a dispatch on a bad router reply).
- `prompt` → sent to the handling agent's session.

> v1 parses JSON from the router's text reply. A stricter structured-output
> binding (a forced tool) is a later hardening, not required for v1.

## 6. Session registry & candidates

- **Keyed bindings**: `(subscriptionName, key) → sessionID`, persisted.
- **Routed candidates**: the registry lists sessions this subscription created,
  newest-active first, capped at `Router.MaxCandidates`, each rendered as a
  `SessionSummary` (§2) via `SessionDriver.SessionSummary`. **No coarse
  pre-filter in v1** — the recency cap is the only bound (a coarse `Candidate(event)`
  key is a later optimization, §11).
- Summaries are **derived from the event log** (first user.message + last
  agent.message) — zero extra storage, no extra LLM. A v2 may let the handling
  agent maintain a one-line summary via a tool (§11).

## 7. Dispatch pipeline

```
ingest(e):
  if e.Hop > MaxHop: reject (loop guard)
  for sub in subscriptions where sub.Match(e):
     if dedup(sub).seen(e.ID): continue          // at-least-once → process once (within window)
     dedup(sub).record(e.ID)
     sid := resolve(sub, e)                        // per policy (§4/§5)
     driver.SendUserMessage(sid, prompt)
     result.record(sub, e.ID, sid)                 // session stays addressable
```

- `resolve` is the only policy-specific step; everything else is shared.
- Same-session turn ordering is **guaranteed by hetairoi** (per-session FIFO
  turn serialization already exists) — the bus does not need its own per-session
  queue. The bus only serializes its own dispatch enough to preserve send order
  for a given session (single dispatch path per subject; see §10).

## 8. Closed loop (deferred — see §11)

A handling agent triggers a new event via an `emit_event(type, subject, payload)`
tool, which posts `Event{Hop: e.Hop+1, CauseID: e.ID}` back to the bus. The
`Hop > MaxHop` guard (§7) prevents runaway loops; `CauseID` traces the chain.
**`emit_event` is a custom tool and depends on the custom-tools (D) work**, so
the loop is deferred — but the `Event` model and the Hop guard are in place now,
so it lights up when D lands.

## 9. Dedup & persistence

- **at-least-once**, **no retries**. Dedup makes processing once-per-window.
- Per-**handler** dedup store: `<state-dir>/eventbus/<subscription-name>/seen.log`
  — a bounded, append-then-rotate window of seen `Event.ID`s.
- `DedupConfig`: window by **count** or **TTL**, per-handler configurable.
- **Rotate**: exceed the window → roll a segment, drop the oldest.
- **Semantic consequence (explicit)**: dedup holds only within the window. An
  event redelivered after its ID rotated out is reprocessed. Size the window to
  cover the source's real redelivery interval (webhooks: seconds–minutes).
- Dedup survives restart (persisted).

## 10. Concurrency & ordering

- Events for **different** sessions dispatch concurrently.
- Events for the **same** Keyed/Routed session: hetairoi serializes the turns,
  so correctness holds even if dispatch races. To also preserve *order*, the bus
  serializes dispatch per resolved session (or per Subject) — a single in-flight
  resolve+send per session. Routed adds a wrinkle: the router call is slow (an
  LLM turn); two near-simultaneous events for the same logical subject could both
  resolve to "new". v1 accepts this (documented); a per-Subject dispatch lock is
  the mitigation if it bites.

## 11. Out of scope (v1) / deferred

- **Closed loop / `emit_event` tool** — needs custom tools (D). Model + Hop guard ready.
- **Coarse candidate key** for Routed — recency cap only in v1.
- **Agent-maintained summaries** (v2) — derived summaries in v1.
- **Retries / dead-letter** — at-least-once + no retry in v1.
- **Non-webhook sources** (MQ, cron, agent-to-agent) — webhook only in v1.
- **Stricter structured output** for the router — JSON-from-text parse in v1.

## 12. Acceptance scenarios (the TDD test list)

Bus / dispatch:
1. An event is delivered only to subscriptions whose `Match` returns true; multiple matches all fire.
2. Dedup: the same `Event.ID` is processed once within the window; distinct IDs each process.
3. Dedup survives a restart (persisted); after the window rotates, a re-seen ID processes again.
4. `Hop > MaxHop` → event rejected, nothing dispatched.

Policies:
5. Stateless: each event creates a new session and sends `Prompt(event)`.
6. Keyed: two events with the same `Key` reuse one session; a new `Key` creates a new one; an archived bound session is replaced by a new one.
7. Routed (reuse): router returns a valid `reuse_session_id` → that session is reused and `prompt` is sent.
8. Routed (new): router returns empty `reuse_session_id` → a new session is created.
9. Routed (degrade): router reply is unparseable / names an unknown or archived session → a new session is created, no crash.
10. Routed (candidates): the candidate list is capped at `MaxCandidates`, newest-active first, each carrying a derived summary (seed + last).

Registry / driver:
11. `SessionSummary` is derived from the event log (first user.message = seed, last agent.message = last).
12. A dispatched event's resolved `sessionID` is recorded and retrievable (so a human can continue the session).

Webhook:
13. `POST` a well-formed event → 2xx and the event is dispatched; a malformed body → 4xx, nothing dispatched.

> The bus is tested against a **fake `SessionDriver`** (records CreateSession /
> SendUserMessage / scripted RunForReply), so policy + dispatch logic is unit-tested
> without a real agent. A thin integration test wires the real driver.
