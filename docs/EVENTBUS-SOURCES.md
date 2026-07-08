# Event Bus — poll sources & the dynamic control plane

This extends [`EVENTBUS-SPEC.md`](EVENTBUS-SPEC.md) (handlers, policies, dedup,
webhook) with two capabilities:

1. **Poll sources** — the *pull* counterpart to the webhook, so hetairoi can
   ingest events from upstreams that can't call us (e.g. an CodeHub project).
2. **A runtime control plane** — `POST/GET/DELETE /v1/eventbus/{sources,handlers}`
   so monitoring is configured with HTTP requests, not recompiled-in Go.

Both are mounted by `cmd/hetairoi` (built-in). Worked example:
[`example/eventbus-dynamic`](../example/eventbus-dynamic). The compiled-in
counterpart is [`example/pr-review`](../example/pr-review).

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
on with `payload_equals`: `has_agent_build_label`, `is_agent_pr` (head branch
starts with `agent_prefix`), `issue_ref` (parsed from the PR body's `Fixes #N`),
`review_verdict` (`approved`/`changes`, parsed from the verdict marker).

| field               | meaning                                                          |
|---------------------|------------------------------------------------------------------|
| `repo`              | `owner/name` (required)                                          |
| `kinds`             | `both` (default) \| `issue` \| `pr`                              |
| `state`             | `open` (default) \| `closed` \| `all`                           |
| `allow_numbers`     | if set, only these numbers are emitted (blast-radius guard)      |
| `interval`          | Go duration, default `30s`                                       |
| `token_file`        | path to a file holding the PAT (else `GITHUB_TOKEN`/`GH_TOKEN`)  |
| `build_label`       | label that opts an issue into the loop, default `agent-build`    |
| `agent_prefix`      | head-branch prefix marking a loop PR, default `agent/`           |
| `bot_marker`        | issue-comment self-trigger marker, default `<!-- cma-agent -->`  |
| `issue_event_type`  | default `issue`                                                  |
| `push_event_type`   | default `pr.push`                                                |
| `review_event_type` | default `pr.review`                                              |
| `api_base`          | default `https://api.github.com` (override for GHE / tests)      |

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

The sanctioned widening is the **`filesystem.shell_access`** card knob (added in
ahsir). hetairoi maps agent **metadata** to it:

| agent metadata key | card field             | effect                         |
|--------------------|------------------------|--------------------------------|
| `shell_access: "true"` | `filesystem.shell_access` | adds `Bash` to allowedTools |
| `runtime_timeout: "900s"` | `runtime.timeout`   | widen ahsir's 120s turn cap    |

```sh
curl -X POST :8787/v1/agents -d '{"name":"pr-reviewer","model":{"id":"claude-sonnet-4-6"},
  "system":"<review procedure>","metadata":{"shell_access":"true","runtime_timeout":"900s"}}'
```

Without `shell_access` the agent can read/edit files but cannot run `git` or
`codehub` — so an action-taking review agent must set it.

## Admin token

hetairoi auto-discovers the ahsir control-plane token like the ahsir CLI:
`CMA_AHSIR_ADMIN_TOKEN` → `AHSIR_ADMIN_TOKEN` → the `admin-token` file beside the
ahsir config (`CMA_AHSIR_CONFIG`, default `~/.ahsir/admin-token`). For a local
same-user setup, no token wiring is needed.
