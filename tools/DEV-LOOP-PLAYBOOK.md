# Multi-agent dev loop — playbook (issue → code → PR → review → fix → merge)

Reproducible recipe for the autonomous coding loop. Built + validated 2026-07-03
against `wu8685/ahsir` (issue #1 → PR #2 → reviewer APPROVED, all autonomous).
Goal of this doc: spin the **same formation** back up fast, without re-hitting the
gotchas below.

## Stack it runs on
- cma-service `127.0.0.1:18790`, ahsir scheduler `:9800`, ahsir UI `:19801` — all
  login LaunchAgents under `~/.cma-stack/` (see the cma-service-autostart memory).
- The `github` eventbus source lives in cma-service (`internal/eventbus/source_github.go`,
  3-event model). Rebuild the deployed binary after code changes:
  `GO111MODULE=on go build -o ~/.cma-stack/bin/cma-service ./cmd/cma-service && codesign --force --sign - ~/.cma-stack/bin/cma-service && launchctl kickstart -k gui/501/com.wu8685.cma-service`

## The formation (what setup-dev-loop.py creates)
- **agents** (opus, `metadata.shell_access=true`, `runtime_timeout` 1800–2400s):
  `ahsir-coder` (implement/fix, push, open PR), `ahsir-reviewer` (build/test + verdict comment).
  **Symmetric test-coverage duty:** the coder MUST ship tests that exercise its own new/changed
  logic (verified via `go test -coverprofile` before pushing; untested logic = don't open the PR).
  The reviewer's remit explicitly includes **test-coverage adequacy** — not just "tests pass" but
  that the PR adds tests exercising the new/changed behavior (measured via `go test -coverprofile`
  + `go tool cover -func`); missing tests on new logic ⇒ `changes` verdict. Pure-UI changes are
  flagged as out-of-headless-scope for a human / Playwright ui-verify.
- **handlers** (keyed): `ahsir-build` (`type=issue` + `has_agent_build_label=true` → coder, key `issue-{{.subject}}`),
  `ahsir-review` (`type=pr.push` + `is_agent_pr=true` → reviewer, key `pr-{{.subject}}`),
  `ahsir-fix` (`type=pr.review` + `review_verdict=changes` → coder, key `issue-{{.payload.issue_ref}}` — same session as build).
- **source** `gh-ahsir-loop` (`type=github`, repo, `kinds=both`, `interval=2m`, `token_file=~/.cma-stack/github-token`).
- **NO** `approved` handler → reviewer's `approved` verdict HALTS the loop for human merge.

## Run it
```sh
# 0. ensure the 3-event source is in the deployed binary (rebuild+restart if source_github.go changed)
# 1. verify PAT write scope on the target repo (avoids a wasted 403 turn):
#    create+delete a temp branch ref via the API (Contents:write); Issues+PR write also needed.
# 2. create the formation (edit REPO at top of the script for a different repo):
python3 ~/.cma-stack/tools/setup-dev-loop.py
# 3. kick off: open an issue on the repo with the `agent-build` label. The 2m poll picks it up.
# 4. watch: GitHub PR list, ahsir UI :19801, or `tail -f ~/.cma-stack/logs/ahsir.err`.
```
Pause the loop (keeps handlers/agents): `curl -s --noproxy '*' -X DELETE http://127.0.0.1:18790/v1/eventbus/sources/gh-ahsir-loop`

## Gotchas (the expensive-to-rediscover ones)
1. **Loopback proxy.** This box has `http_proxy=127.0.0.1:7897`; loopback dies unless
   `export no_proxy=127.0.0.1,localhost` per process and `curl --noproxy '*'`. External
   (GitHub API) still uses the proxy. All LaunchAgent plists already bake in no_proxy.
2. **Single GitHub account can't self-approve.** GitHub blocks approve/request-changes
   on your OWN account's PR. So routing is by **event type** (coder⇒`pr.push`, reviewer⇒`pr.review`)
   + a **verdict marker in a PR comment** (`<!-- cma-review:approved -->` / `<!-- cma-review:changes -->`),
   NOT native review state. Event-type separation also prevents self-triggering.
3. **CMA_TURN_TIMEOUT default 10m is too short** for real coding turns → set `CMA_TURN_TIMEOUT=45m`
   in the cma-service plist + `runtime_timeout` (e.g. 2400s) on the agent. Changing plist env
   needs `launchctl bootout gui/501/<label>` then `bootstrap` (kickstart does NOT reload env).
4. **PAT scope.** Fine-grained token needs the target repo with Contents + Pull requests + Issues
   write. Probe first: create+delete a temp ref (Contents:write); a read-only token 403s on the first push.
5. **merged ≠ deployed.** The loop clones the GitHub repo; the running ahsir stack is built from
   local `~/workspace/.../ahsir`. Merging to GitHub main does NOT change the running scheduler/UI —
   rebuild `~/.cma-stack/bin/{ahsir,ahsir-agent}` from the updated source + restart to deploy.
6. **Stale handlers conflict.** A handler matching `type=issue` with no label filter will intercept
   the new repo's issues. Delete old repo-specific handlers before wiring a new repo's loop.
7. **Re-trigger a handled issue**: toggle its label (remove+re-add `agent-build`) → bumps updated_at
   → new Event.ID → re-fires. Dedup is per Event.ID (persisted in `~/.cma-stack/eventbus/<handler>.json`).
   Source `since` starts at `now` on (re)start, so it never replays history — only new activity fires.
8. **UI features need real verification.** The reviewer agent is headless (no browser) → it verifies
   build+test+code only, NOT rendered UI behavior. Use **Playwright** (Python; system Chrome headless
   HANGS on this box — Playwright's bundled chromium is ~2s reliable). `ui-verify.py` builds a PR branch,
   runs an isolated scheduler+UI, seeds state, drives the click-through, asserts + screenshots.
   `python3 ~/.cma-stack/tools/ui-verify.py <owner/repo> <branch> <out.png>`. Reviewer can act on its
   PASS/FAIL text but can't "see" the screenshot — visual judgment still needs a human.
9. **ahsir agent transcripts survive deletion** (`.a2a/transcripts/*.jsonl`, per-completed-turn), but the
   UI only lists registered agents. To view a deleted agent: re-register via `POST /admin/agents`
   `{name, workspace}` (no card → reuses the existing workspace, no re-scaffold). 30-day retention
   (`CompactForRetention` at agent startup) prunes older transcripts. (PR #2 added an "Archived" UI for this.)

## Cost/safety
- Each coder/reviewer turn ≈ a multi-minute opus turn ($). A full loop = several turns.
- Guards: `agent-build` label gate, optional `allow_numbers` on the source, the 5-round self-cap in
  the coder prompt, and the human merge gate. Keep the source paused when not actively using it.
