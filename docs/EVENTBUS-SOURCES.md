# Event Bus — poll sources & the dynamic control plane

This extends [`EVENTBUS-SPEC.md`](EVENTBUS-SPEC.md) (handlers, policies, dedup,
webhook) with two capabilities:

1. **Poll sources** — the *pull* counterpart to the webhook, so hetairoi can
   ingest events from upstreams that can't call us (e.g. an CodeHub project).
2. **A runtime control plane** — `POST/GET/DELETE /v1/eventbus/{sources,handlers}`
   so monitoring is configured with HTTP requests, not recompiled-in Go.

Both are mounted by `cmd/hetairoi` (built-in). The current end-to-end worked
example is the autonomous dev loop: `tools/DEV-LOOP-PLAYBOOK.md` +
`tools/setup-dev-loop.py` (two-port: CMA calls → the ahsir facade, eventbus wiring
→ Hetairoi).

## Sources

A `Source` runs until cancelled, pushing events into the bus. `WebhookHandler`
is push; `Poller` is the generic pull source — every `interval` it calls a
`FetchFunc` and dispatches each event.

**Dedup is the bus's job.** A poller re-emits the same items every tick; the bus
drops any `Event.ID` it has seen. So a source must encode the item's *mutable
version* into the ID. `CodeHubPRSource` uses `pr-<iid>-<head_sha>`:

- unchanged PR → same ID → deduped → **no work, no backlog**;
- a fix push → new head sha → new ID → **exactly one re-review** (and, keyed by
  iid, in the same session as before).
- the agent's own comment does **not** change the head sha, so it never
  self-triggers (using `updated_at` would loop).

### `codehub-pr` source

Lists PRs where `reviewer` is a reviewer and emits one `pr` event per (allowed)
PR. Shells out to the CodeHub CLI (v1.x), which carries its own auth.

| field          | meaning                                                       |
|----------------|---------------------------------------------------------------|
| `project`      | `namespace/project` (required)                                |
| `reviewer`     | reviewer filter, e.g. `@me` (empty = no filter)               |
| `author`       | author filter, e.g. `@me` (PRs I created)                     |
| `state`        | PR state, default `opened`                                    |
| `interval`     | Go duration, default `30s`                                    |
| `allow_iids`   | if set, only these PR iids are emitted (blast guard)          |
| `merge_status` | also fetch `mergeable`/`merge_status` (one extra `pr show` per PR) into the payload AND fold `merge_status` into `Event.ID` — so a PR that *becomes* conflicted (target advanced) re-fires even with an unchanged head sha |
| `event_type`   | emitted `Event.Type`, default `pr`                            |
| `bin`          | codehub binary, default `codehub`                             |

**Conflict-fixer pattern**: `author: "@me"` + `merge_status: true` + a handler matching
`payload_equals: { mergeable: "false" }` drives an agent only on *your own conflicted*
PRs — e.g. clone, rebase onto target, resolve, `go build`+`go test`, push a
`<branch>-autofix` branch + comment. Such an agent needs `shell_access` and a longer
turn budget (`CMA_TURN_TIMEOUT`, since the default turn cap is 10m).

### `workitem` source

Polls workitem work items (via the `workitem` CLI) in a space/project and emits one event
per item, keyed by the item's mutable version. workitem's JSON schema varies by
deployment, so the id/version field names are **configurable** with default
candidate lists — validate the mapping against your own space the first time.

| field            | meaning                                                       |
|------------------|---------------------------------------------------------------|
| `space`          | `-s <workspaceId>` (space or project required)                |
| `project`        | `-p <projectId>`                                              |
| `scope`          | `--scope`, default `personal` (my items)                      |
| `belong`         | `--belong` (`Workitem`/`Task`/`Req`/`Bug`/…)                  |
| `status_list`    | `--status-list` filters                                       |
| `page_size`      | default 50                                                     |
| `id_field`       | JSON key for the item id (default tries `serialNumber`/`id`/…) |
| `version_field`  | JSON key for the dedup version (default `gmtModified`→`status`)|
| `event_type`     | emitted `Event.Type`, default `workitem`                          |

Event ID is `workitem-<id>-<version>`, so an unchanged item is deduped and a changed
one re-triggers — same contract as `codehub-pr`.

### `github` source

Polls the GitHub REST API for a repo's issues + PRs and emits **three typed
events**, purpose-built for the issue → code → PR → review → fix loop. Each event
type has its own dedup key so a transition fires exactly once:

| event type | dedup `Event.ID`             | fires when                          |
|------------|------------------------------|-------------------------------------|
| `issue`    | `gh-issue-<n>-<activity>`    | a (labeled) issue changed           |
| `pr.push`  | `gh-pr-<n>-push-<head_sha>`  | new commits on a PR (open/sync)     |
| `pr.review`| `gh-pr-<n>-review-<comment>` | a reviewer verdict comment appeared |

`Subject` = the issue/PR number. Payload carries the routing fields handlers match
on with `payload_equals`: `authorized` (the approval gate — **build handlers must
match this, not the raw label**, see the trust boundary below), `has_agent_build_label`,
`is_agent_pr` (head branch starts with `agent_prefix`), `issue_ref` (parsed from the
PR body's `Fixes #N`), `review_verdict` (`approved`/`changes`, parsed from the
verdict marker).

| field               | meaning                                                          |
|---------------------|------------------------------------------------------------------|
| `repo`              | `owner/name` (required)                                          |
| `owner`             | trusted owner login; default: the owner segment of `repo`        |
| `kinds`             | `both` (default) \| `issue` \| `pr`                              |
| `state`             | `open` (default) \| `closed` \| `all`                           |
| `allow_numbers`     | if set, only these numbers are emitted (blast-radius guard)      |
| `interval`          | Go duration, default `30s`                                       |
| `token_file`        | path to a file holding the PAT (else `GITHUB_TOKEN`/`GH_TOKEN`)  |
| `build_label`       | label that opts an issue into the loop, default `agent-build`    |
| `agent_prefix`      | head-branch prefix marking a loop PR, default `agent/`           |
| `approve_marker`    | owner approval marker for a non-owner issue, default `<!-- cma-approve -->` |
| `bot_marker`        | issue-comment self-trigger marker, default `<!-- cma-agent -->`  |
| `issue_event_type`  | default `issue`                                                  |
| `push_event_type`   | default `pr.push`                                                |
| `review_event_type` | default `pr.review`                                              |
| `api_base`          | default `https://api.github.com` (override for GHE / tests)      |

**Trust boundary — issue ingestion is an auto-execute attack surface.** The label→
code→PR loop runs untended, so any account that can shape an issue's content, its
label, or a comment could try to steer an agent. The source enforces an
**owner-only** trust boundary at ingestion, keyed off the `owner` login:

- **Owner-only content.** Only comments authored by `owner` are surfaced to agents
  (`latest_comment`). A non-owner comment is never fed to an agent and never becomes
  the trigger — it is logged as a probe and dropped. The source only reads issue/PR
  text over the REST API; it **never downloads or executes** attachments or links in
  any comment.
- **Approval gate.** The `agent-build` label is a *candidate*, not authorization.
  The payload's **`authorized`** is `true` only when the label is present **and** the
  intent is owner-backed — the issue is authored by `owner`, or `owner` posted an
  `approve_marker` comment. A label driven onto a non-owner issue yields
  `authorized=false` (logged). **Build handlers must gate on `authorized: "true"`**,
  not on `has_agent_build_label`, so a label alone cannot start work.
- **Owner-only review verdicts.** A `pr.review` event is emitted only for a verdict
  comment authored by `owner`; a stranger's `approved`/`changes` comment is ignored
  (logged), even if it is newer than the owner's verdict.

Every rejection is logged (`eventbus: github <repo>: …`) so a probe of the
auto-execute entry point is visible.

> **Poll-source limitation.** Because this is a *poller*, it never sees the
> `sender.login` of a GitHub `labeled` webhook event, so it cannot tell *who*
> applied the label. Authorization is therefore derived from verifiable
> owner-authored content (issue author, or an owner `approve_marker` comment) rather
> than the labeler's identity.

**Auth.** A token is **required** (read from `token_file`, never stored in the
persisted spec): unauthenticated polling is capped at 60 req/hr, and the source
resolves the bot login via `GET /user`. For a full coding loop the PAT needs, on
the target repo: **Contents + Issues + Pull requests: Read and write** (clone,
push, open PRs, comment). Probe write access first with a temp-branch create/delete.

**Routing without author identity (single-account safe).** The coder only ever
produces `pr.push`, the reviewer only ever produces `pr.review`, so routing on the
event *type* never self-triggers even when both act as the same GitHub account.
GitHub blocks approving your own PR, so the reviewer posts a verdict as a PR
comment ending with `<!-- cma-review:approved -->` or `<!-- cma-review:changes -->`
and the source exposes it as `review_verdict`. The `issue` event keeps a
`bot_marker` guard (skip when the newest comment carries the marker) so an agent
comment on an issue doesn't re-trigger it.

**No boot replay.** On (re)start the window begins at `now`, so the source acts
only on activity *after* startup — a restart never replays history.

See [`tools/DEV-LOOP-PLAYBOOK.md`](../tools/DEV-LOOP-PLAYBOOK.md) (+ `tools/setup-dev-loop.py`,
`tools/ui-verify.py`) for the full agent/handler formation this source drives.

### `exec` source — pluggable, code-free sources

The built-ins above each needed a Go `FetchFunc` + a `case` in `buildFetch`. `exec`
removes that: it is a **generic** source that shells out to any command every
`interval` and reads a JSON array of events from its **stdout**. A new upstream is
now a *script + a POST*, never a recompile.

The division of responsibility is the same as every source — the plugin just owns
the fetch:

- **Plugin owns**: how to talk to the upstream, and — critically — **encoding the
  mutable version into `id`** (`pr-<iid>-<sha>`, `prcomment-<iid>-<maxCommentId>`).
  That is the whole basis of *fire once per change*, since the bus dedups by `id`.
- **hetairoi owns**: interval scheduling, dedup, handler routing, `_registry.json`
  persistence — unchanged. It fills `source` and `time` on each emitted event.

| field        | meaning                                                          |
|--------------|------------------------------------------------------------------|
| `command`    | argv, e.g. `["python3","/opt/het/pr_comment.py","--project","org/repo"]` (required) |
| `env`        | extra env vars (`{"no_proxy":"*"}`) merged over the inherited environment |
| `interval`   | Go duration, default `30s`                                       |
| `event_type` | default `Event.Type` for events that omit their own `type`       |

**Spec:**
```jsonc
{
  "name": "pr-comments",
  "type": "exec",
  "interval": "60s",
  "command": ["python3", "/opt/hetairoi/sources/pr_comment.py",
              "--project", "org/repo", "--author", "@me"],
  "env": { "no_proxy": "*" },
  "event_type": "pr.comment"
}
```

**Plugin stdout — a JSON array of events** (`id` required; `type` falls back to the
spec's `event_type`; `subject`/`payload` are the routing key + body handlers match
and template over):
```json
[
  {"id":"prcomment-3196-213601693","type":"pr.comment","subject":"3196",
   "payload":{"iid":3196,"project":"org/repo","comment_note":"..."}}
]
```

**Environment the plugin sees.** In addition to the inherited environment and any
`env` from the spec, hetairoi injects:

| var           | meaning                                                            |
|---------------|--------------------------------------------------------------------|
| `HET_PROTOCOL`| event-contract version (currently `1`) — gate your output on it     |
| `HET_SCRATCH` | a per-source scratch dir (also the command's working dir) — persist a cursor here for stateful sources (github's `since`); stateless sources (the codehub re-emit-all pattern) need nothing |

**Failure handling** mirrors the built-in pollers: a non-zero exit or malformed
JSON is returned as a fetch error (logged, dispatches nothing); an entry with no
`id` is skipped (no stable dedup identity).

**Trivially testable.** A plugin is a script you can run by hand —
`python3 pr_comment.py … | jq` — without booting hetairoi.

> **Security.** `exec` runs an arbitrary command with hetairoi's privileges.
> Acceptable for a local, single-user tool behind a loopback control plane, but
> source specs must come from a **trusted operator**. The event JSON
> (`id`/`type`/`subject`/`payload`) is a public interface — keep it minimal and
> version it via `HET_PROTOCOL`.

A minimal reference plugin lives at
[`tools/sources/example_exec_source.py`](../tools/sources/example_exec_source.py).

## Dynamic control plane

| method + path                       | body / effect                          |
|-------------------------------------|----------------------------------------|
| `POST /v1/eventbus/sources`         | `SourceSpec` → start + persist a source |
| `GET /v1/eventbus/sources`          | `{data:[SourceSpec…]}`                  |
| `DELETE /v1/eventbus/sources/{name}`| stop + forget                          |
| `POST /v1/eventbus/handlers`        | `HandlerSpec` → register + persist      |
| `GET /v1/eventbus/handlers`         | `{data:[HandlerSpec…]}`                 |
| `DELETE /v1/eventbus/handlers/{name}`| unregister + forget                   |

Specs are persisted to `<state-dir>/eventbus/_registry.json` and **rebuilt on
boot** (handlers first, then sources), so a long-running hetairoi keeps its
wiring across restarts.

### Declarative handlers (no closures)

A `Subscription`'s `Match`/`Key`/`Prompt` are Go funcs in code. Over the wire
they become data:

```jsonc
{
  "name": "pr-review",
  "match": {                       // ANDed; empty matches all
    "type": "pr",                  // exact Event.Type
    "subject_glob": "31*",         // path.Match over Event.Subject
    "payload_equals": {"meta.iid": "3177"}  // dotted path in payload → string
  },
  "policy": {
    "kind": "keyed",               // stateless | keyed | routed
    "agent_id": "agent_…",         // validated to exist at create time
    "version": 0,                  // 0 = latest
    "env_id": "env_…",
    "key_template": "{{.subject}}",            // keyed only
    "prompt_template": "Review PR {{.payload.iid}}"
  },
  "dedup": {"max_entries": 1024, "ttl": "0s"}
}
```

- **match**: a struct matcher (no expression-language dependency).
- **key/prompt**: Go `text/template` over the event view —
  `.id .type .subject .source .hop .cause_id .payload`. `.payload` is the decoded
  JSON object, so `{{.payload.iid}}` works. A render error degrades to `""`.
- **routed** adds a `router` object (`agent_id`, `system_prompt`,
  `max_candidates`).

## Agents that act: `shell_access`

A handler agent that does the work itself (clone, run a CLI, post results) needs
the **Bash** tool. ahsir deliberately withholds Bash from claude agents — the
`--allowedTools` whitelist is `Read,LS,Glob,Grep` (+`Edit,MultiEdit,Write` with
write access), and `--dangerously-skip-permissions` is stripped from raw args.

The sanctioned widening is the **`filesystem.shell_access`** card knob (in ahsir).
It's set through the agent's `metadata` when the agent is created **on the ahsir CMA
facade** (agents live there, not on Hetairoi):

| agent metadata key | card field             | effect                         |
|--------------------|------------------------|--------------------------------|
| `shell_access: "true"` | `filesystem.shell_access` | adds `Bash` to allowedTools |
| `runtime_timeout: "900s"` | `runtime.timeout`   | widen ahsir's default turn cap |

```sh
# create the agent on the ahsir facade (CMA API), not on Hetairoi:
curl -X POST http://127.0.0.1:18790/v1/agents -H "x-api-key: $CMA_API_KEY" \
  -d '{"name":"pr-reviewer","model":{"id":"claude-sonnet-4-6"},
       "system":"<review procedure>","metadata":{"shell_access":"true","runtime_timeout":"900s"}}'
```

A handler on Hetairoi then references that agent's id in its policy; the SDK driver
creates sessions for it on the facade. Without `shell_access` the agent can read/edit
files but cannot run `git`/`codehub` — so an action-taking agent must set it.

## Facade auth

Hetairoi reaches ahsir through the CMA facade, authenticated with `CMA_API_KEY`
(`CMA_FACADE_URL` points at the facade). It no longer talks to ahsir's scheduler
admin surface directly, so no ahsir admin token is wired into Hetairoi — the facade
handles agent registration on ahsir's side. For a local same-user setup, an empty
key is fine (the facade is open to loopback).
