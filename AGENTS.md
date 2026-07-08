# hetairoi — agent orientation (Codex)

Equivalent to `CLAUDE.md`; kept in sync for Codex.

**What this is:** a gateway exposing an **Anthropic Managed Agents (CMA) compatible
HTTP API**, backed by an [ahsir](https://github.com/wu8685/ahsir) agent fleet. The
official `anthropic` SDK pointed at `base_url` drives it as a drop-in.

**Read first:** `docs/DESIGN.md` and `docs/ROADMAP.md`.

## Repo map

```
cmd/hetairoi/      entrypoint
internal/cma/         CMA wire types (external API surface) + ids
internal/ahsir/       ahsir gateway client + inline AgentCard
internal/translate/   CMA agent → ahsir card; (agent_id,version) → ahsir name
internal/store/       resources + session↔(name,contextId) + event bus (JSON persist)
internal/api/         routing + auth + handlers + turn execution
e2e/                  official-SDK end-to-end tests + fakeahsir backend
```

## Build / test (this machine)

- Module sits in `GOPATH/src` with `GO111MODULE=off` global → **prefix with `GO111MODULE=on`**.
- This Mac **SIGKILLs directly-run freshly-built binaries** → use **`go run`**, not a built binary.

```sh
GO111MODULE=on go build ./...
GO111MODULE=on go vet ./...
GO111MODULE=on go run ./cmd/hetairoi
python3 -m pip install -r e2e/requirements.txt
./e2e/run.sh
```

## Constraints

1. **Keep response shapes aligned to the installed `anthropic` SDK** — introspect its
   pydantic models, do not guess (recipe in `docs/ROADMAP.md`). Required-field gotchas
   in `docs/DESIGN.md`.
2. **Versioning lives here**, not ahsir: `(agent_id, version)` → ahsir agent `cma-<id>-v<n>`.
3. **Any ahsir-side change is discussed with the ahsir maintainer first** — ahsir stays a pure agent
   runtime. Pending change (inline agent registration) specced in `docs/ROADMAP.md`.
4. **`e2e/` must stay green** — it's the contract check against the real SDK.

## Status

P1 MVP done, e2e 7/7 green. Next: P2 (real-ahsir e2e, interrupt, token-incremental
streaming). See `docs/ROADMAP.md`.
