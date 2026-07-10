# Hetairoi

**Connect real-world events to your agent fleet.** Hetairoi watches the sources
you care about — GitHub issues and PRs, code-review queues, work items — and, when
something happens, **summons the right agent to act on it**. Autonomously.

Point it at an [ahsir](https://github.com/wu8685/ahsir) agent fleet and Hetairoi
becomes the layer that turns "an issue was labeled" or "a PR was pushed" into "an
agent picked it up, did the work, and reported back."

> *Ἑταῖροι* — Iskandar's Companion cavalry, the army that materializes to answer
> the king's call (Fate/Zero, «Ionioi Hetairoi»). ahsir is the fleet; Hetairoi is
> what rallies it when the world calls.

## What you get

- **Watch anything.** Built-in sources poll GitHub / CodeHub / work-item queues;
  a webhook lets anything else push events in; and a pluggable `exec` source turns
  *any script that prints JSON* into a source — connect a new upstream with a file
  and a POST, no recompile.
- **Route by rule, not by code.** Declarative handlers match an event and hand it
  to a policy — start a fresh session, reuse a keyed one, or let a router agent
  decide — no redeploy to change the wiring.
- **Agents that actually do the work.** Each event becomes a real agent turn on
  your ahsir fleet: a coder implements, a reviewer reviews, a triager triages.
- **Runs itself.** Dedup, per-session ordering, crash-safe event logs, and a human
  hand-off gate come out of the box.

## The flagship: an autonomous dev loop

Label a GitHub issue `agent-build` and walk away:

1. Hetairoi sees the labeled issue → a **coder** agent implements it, opens a PR.
2. The PR push → a **reviewer** agent builds, tests, and posts a verdict.
3. `changes requested` → the coder revises; `approved` → Hetairoi **stops** and
   waits for a human to merge.

No glue scripts, no CI plumbing — just agents answering events.

## How it fits

```
GitHub / CodeHub / webhooks ─▶ Hetairoi (eventbus) ─▶ ahsir CMA facade ─▶ agent fleet
        (the world)              match → policy          (runs the turn)     (does the work)
```

Hetairoi is a **client** of ahsir's agent API — it drives the fleet through the
official Anthropic SDK. Your agents, their sessions, and their state live on ahsir;
Hetairoi is the trigger-and-orchestrate layer in front of them.

## Get started

Run `ahsir` (with its CMA facade) and point Hetairoi at it:

```sh
CMA_FACADE_URL=http://127.0.0.1:18790 \
CMA_LISTEN=127.0.0.1:18791 \
GO111MODULE=on go run ./cmd/hetairoi
```

Then declare sources and handlers over the control-plane API (`/v1/eventbus/…`) —
see **[docs/EVENTBUS-SOURCES.md](docs/EVENTBUS-SOURCES.md)** for sources and the
dynamic control plane, and **[docs/EVENTBUS-SPEC.md](docs/EVENTBUS-SPEC.md)** for the
policy model. Architecture and design rationale live in
**[docs/DESIGN.md](docs/DESIGN.md)**.
