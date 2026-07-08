# RFC-001 — Move the CMA gateway into ahsir; cma-service becomes a CMA-SDK scenario layer

Status: **DRAFT — awaiting 昊天/owner sign-off** (this reverses two documented principles;
see §7). Author: agent, 2026-07-08.

## 1. Motivation

Today the split is:

- **ahsir** — pure multi-agent runtime (scheduler + agent wrappers, A2A transport, admin
  agent registration). CMA-agnostic.
- **cma-service** — an HTTP gateway that exposes the **CMA (Anthropic Managed Agents)**
  wire API and translates it onto ahsir's A2A/admin surface. Also hosts an **eventbus**
  (github/codehub/workitem sources + handler→policy→session/turn) that is really a set of
  *use-case scenarios* (e.g. the autonomous dev loop).

The owner wants to re-cut the boundary so it matches responsibility:

> ahsir 负责多 agent 调度；cma-service 负责和具体的**使用场景**做对接。
> cma 的**网关能力也迁移到 ahsir** 去；cma-service 后续调用 ahsir **都用 CMA SDK**，
> 顺便验证链路。

So the CMA API stops being a separate front process and becomes **ahsir's own public
face**. cma-service is demoted from "gateway" to "**scenario orchestrator** that drives
ahsir through the CMA SDK" — which also **dogfoods** the CMA API (if our own eventbus can
drive ahsir purely through the SDK, the CMA surface is provably complete).

```
BEFORE                                   AFTER
official SDK ─▶ cma-service ─A2A▶ ahsir   official SDK ─▶ ahsir(CMA facade)─in-proc▶ scheduler
                    ▲                                          ▲
              eventbus (in same proc)                    cma-service eventbus ─CMA SDK┘
                                                         (scenario layer, a CMA *client*)
```

## 2. End-state architecture

**ahsir** (gains a CMA facade, core stays runtime-only):
- `internal/scheduler`, `internal/wrapper` — **unchanged**, remain CMA-agnostic.
- **new** `internal/cmagateway/` — the CMA facade. Absorbs, from cma-service:
  `internal/cma` (wire types + ids), `internal/translate` (CMA agent→ahsir card;
  `(agent_id,version)`→`cma-<id>-v<n>`), `internal/store` (resources + session↔(name,contextId)
  + event bus + JSON persist), and the CMA request handlers/turn-execution from
  `internal/api` (`handlers.go`, `server.go`). It calls the scheduler **in-process** — the
  `internal/ahsir` HTTP client (`a2a.go`, `client.go`) collapses into direct scheduler calls.
- served on its own listener (see Decision A), so the anthropic SDK points at ahsir directly.

**cma-service** (becomes the scenario layer, a CMA client):
- keeps `internal/eventbus/*` (sources github/codehub/workitem, bus, webhook, substore),
  `internal/api/busdriver.go` + `eventbus_admin.go` (the eventbus admin HTTP surface),
  trimmed `internal/config`.
- drives ahsir's CMA API via the **official `anthropic-sdk-go`** directly (Decision B) — no
  bespoke client. The eventbus policy that today does "create session / run turn" against the
  in-process store now does it as `client.Beta.Sessions.New` + `Sessions.Events.Send/StreamEvents`
  against ahsir. A thin adapter may wrap the SDK for config/base_url, but the calls are the SDK's.
- **deleted**: `internal/cma`, `internal/translate`, `internal/store`, `internal/ahsir`,
  and the CMA half of `internal/api`. (These move to ahsir.)

## 3. Open decisions (need your call)

**Decision A — where the CMA facade lives / port strategy.**
- A1 *(recommended)*: new package `internal/cmagateway` served by `cmd/ahsir` on a **separate
  `--cma-listen` port** (e.g. `:18790`, reusing today's cma-service port). Core scheduler
  gateway (`:9800`) stays untouched; the CMA facade is a clean, separable layer that just
  happens to ship in the ahsir repo and call the scheduler in-process. Honors the *spirit*
  of "pure runtime" (core is CMA-agnostic; facade is a distinct package).
- A2: fold CMA `/v1/...` routes into the existing `:9800` scheduler gateway. Fewer ports,
  but mixes CMA wire-shape concerns into the runtime gateway.

**Decision B — how cma-service's eventbus calls ahsir.**
**RESOLVED 2026-07-08 → B1: the official `anthropic-sdk-go` for ALL ahsir calls.**
(An earlier note here claimed the Go SDK lacked sessions — that was WRONG, from a truncated
`api.md` read. Corrected below by inspecting the SDK source directly.)
- **Verified against `anthropic-sdk-go v1.56.0` source** (`go get` + grep): the full
  managed-agents surface is present —
  - `client.Beta.Agents` (New/Update/Get/List/Archive) + `Agents.Versions`
  - `client.Beta.Environments` (New/Update/Get/List/Archive/Delete)
  - `client.Beta.Sessions` (**New/Get/List/Update/Delete/Archive**)
  - `client.Beta.Sessions.Events` (**Send / StreamEvents / List**) ← runs a turn
  - plus `Sessions.Resources`, `Sessions.Threads[.Events]`, `Sessions.ToolRunner`.
- **B1 (chosen)**: cma-service's eventbus drives ahsir **entirely** through this SDK —
  create agent/environment, `Sessions.New`, `Sessions.Events.Send` to run a turn,
  `Sessions.Events.StreamEvents`/`List` to consume the reply. No hand-rolled client.
- **The current `internal/ahsir` A2A/admin client is deleted**, not moved: cma-service
  becomes a pure official-CMA-SDK client of ahsir. Double dogfood — the **ahsir e2e** hits
  the same facade with the **Python** anthropic SDK (§5), so both official SDKs
  (Go + Python) certify the wire.

**Decision C — repo name.** Keep `cma-service` (least churn; it's now "the CMA-driven
scenario runner") vs rename (e.g. `ahsir-scenarios`). Recommend **keep for now**.

## 4. Phased plan (each phase independently shippable + reversible)

- **P0** — this RFC + sign-off. *(no code)*
- **P1** — Stand up the CMA facade **inside ahsir** (copy the 4 packages, wire in-process,
  own port), with the old cma-service gateway still running untouched. Port `e2e/` to point
  the official SDK at **ahsir's CMA port** (needs a fake/echo provider in ahsir for
  deterministic replies, mirroring today's `e2e/fakeahsir`). Gate: e2e green against ahsir.
- **P2** — Refactor cma-service: add `internal/cmaclient`, rewrite the eventbus policy to
  drive ahsir's CMA API via the SDK; delete the migrated gateway packages. Gate:
  `go test ./...` green; the dev-loop scenario runs end-to-end against ahsir's CMA API.
- **P3** — Cut over the running stack (LaunchAgents): ahsir serves CMA on `:18790`;
  cma-service runs eventbus-only and points its `CMA_*` client at ahsir. Verify the live
  dev loop still fires. Rollback = revert P3 (old cma-service binary still gateway-capable
  until P2's deletes land, so keep a tagged pre-P2 binary).

## 5. e2e / contract-check relocation

`e2e/` is the SDK contract check. It moves with the gateway: the official SDK must point at
**ahsir's CMA port**. Since ahsir's facade calls the *real* scheduler in-process, e2e needs
a deterministic provider — port `e2e/fakeahsir`'s echo behavior into an ahsir
`--provider=fake` (or reuse the existing wrapper fake path). Keep the same 9 tests green;
that's the migration's definition of done for P1.

## 6. Risks

- **Wire-shape regressions.** The whole point of cma-service was strict SDK alignment
  (`docs/DESIGN.md` → wire-shape rules). Moving handlers must preserve required-field
  shapes; e2e is the guard — P1 doesn't complete until e2e is green against ahsir.
- **In-process coupling.** Folding the A2A client into direct scheduler calls is a
  simplification but touches turn-execution/cancel/stream semantics; port carefully with
  the existing tests.
- **Live stack downtime.** P3 repoints 3 LaunchAgents + the dev-loop source; do it when no
  session is in-flight (same guard we used for the #13 deploy).

## 7. Principles this RFC updates (needs explicit owner OK)

1. CLAUDE.md constraint #3 — *"ahsir stays a pure agent runtime; any ahsir-side change is
   discussed with 昊天 first."* This RFC **is** that discussion. Core scheduler stays
   runtime-only; the CMA facade is an explicitly separate layer/package (Decision A1).
2. CLAUDE.md constraint #2 — *"Versioning lives here [cma-service], not in ahsir."*
   `translate` (which owns `(agent_id,version)`→`cma-<id>-v<n>`) moves into ahsir's CMA
   facade. Versioning stays a **CMA-facade** concern, not a core-scheduler one — the core
   remains version-agnostic, so the *intent* of #2 holds; the doc wording changes.
```
