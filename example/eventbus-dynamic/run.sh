#!/usr/bin/env bash
# Phase-2 (dynamic control plane) end-to-end: boot the REAL hetairoi (which
# mounts the event-bus registry) + an isolated ahsir, then wire the CodeHub PR
# review entirely over HTTP — create the agent, the source, and the handler with
# curl/urllib, no compiled-in wiring. Proves /v1/eventbus/{sources,handlers}.
#
#   ./run.sh                 # review PR 3177
#   REVIEW_IID=3179 ./run.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
AHSIR=${AHSIR_DIR:-$REPO/../ahsir}

PROJECT=${REVIEW_PROJECT:-example-org/k8s-extension}
IID=${REVIEW_IID:-3177}
MODEL=${REVIEW_MODEL:-claude-sonnet-4-6}
CODEHUB_BIN=${CMA_CODEHUB_BIN:-$HOME/.local/bin/codehub}
AHSIR_PORT=${AHSIR_PORT:-19800}
UI_PORT=${AHSIR_UI_PORT:-19801}
APP_PORT=${APP_PORT:-18790}
PR_LO=19811; PR_HI=19860

export PATH="$HOME/.superset/bin:$HOME/.local/bin:$PATH"
export no_proxy=127.0.0.1,localhost NO_PROXY=127.0.0.1,localhost

command -v claude >/dev/null || { echo "claude CLI not found"; exit 1; }
[ -x "$CODEHUB_BIN" ] || { echo "codehub not at $CODEHUB_BIN"; exit 1; }
"$CODEHUB_BIN" auth whoami >/dev/null 2>&1 || { echo "codehub not logged in"; exit 1; }
[ -d "$AHSIR" ] || { echo "ahsir not at $AHSIR"; exit 1; }
for port in $AHSIR_PORT $UI_PORT $APP_PORT; do
  lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 && { echo "port $port busy"; exit 1; }
done

RUN=$(mktemp -d); echo "logs: $RUN"
cleanup() {
  echo "--- cleanup (only this run) ---"
  [ -n "${UI_PID:-}" ]    && kill "$UI_PID"    2>/dev/null || true
  [ -n "${APP_PID:-}" ]   && kill "$APP_PID"   2>/dev/null || true
  [ -n "${SCHED_PID:-}" ] && kill "$SCHED_PID" 2>/dev/null || true
  pkill -f "registry http://127.0.0.1:$AHSIR_PORT" 2>/dev/null || true
  pkill -f "exe/hetairoi" 2>/dev/null || true
}
trap cleanup EXIT

cat > "$RUN/ahsir.yaml" <<YAML
registry: { host: "127.0.0.1", port: $AHSIR_PORT, heartbeat_interval: 10s, heartbeat_timeout: 30s }
port_range: { start: $PR_LO, end: $PR_HI }
timeouts: { chat: 15m, task_status: 30s }
YAML

echo "--- 1. build + start ahsir ($AHSIR_PORT) + UI ($UI_PORT) ---"
( cd "$AHSIR" && GO111MODULE=on go build -o bin/ahsir ./cmd/ahsir && GO111MODULE=on go build -o bin/ahsir-agent ./cmd/ahsir-agent )
export AHSIR_ADMIN_TOKEN="${AHSIR_ADMIN_TOKEN:-eventbus-dynamic-token}"
"$AHSIR/bin/ahsir" start "$RUN/ahsir.yaml" > "$RUN/ahsir.log" 2>&1 & SCHED_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$SCHED_PID" 2>/dev/null || { echo "scheduler died:"; cat "$RUN/ahsir.log"; exit 1; }
  curl -sf "http://127.0.0.1:$AHSIR_PORT/agents" >/dev/null 2>&1 && break; sleep 0.5
done
"$AHSIR/bin/ahsir" ui --addr "127.0.0.1:$UI_PORT" --scheduler "http://127.0.0.1:$AHSIR_PORT" > "$RUN/ui.log" 2>&1 & UI_PID=$!
echo ">>> ahsir UI: http://127.0.0.1:$UI_PORT"

echo "--- 2. start REAL hetairoi ($APP_PORT) with the eventbus registry ---"
# hetairoi auto-discovers the admin token from AHSIR_ADMIN_TOKEN (Phase-2 #2).
GO111MODULE=on \
  CMA_LISTEN=127.0.0.1:$APP_PORT CMA_AHSIR_URL=http://127.0.0.1:$AHSIR_PORT \
  CMA_RUNTIME_PROVIDER=anthropic CMA_STATE_FILE="$RUN/state.json" \
  go run "$REPO/cmd/hetairoi" > "$RUN/app.log" 2>&1 & APP_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$APP_PID" 2>/dev/null || { echo "hetairoi died:"; cat "$RUN/app.log"; exit 1; }
  curl -sf "http://127.0.0.1:$APP_PORT/v1/agents" >/dev/null 2>&1 && break; sleep 0.5
done

echo "--- 3. wire agent + source + handler over HTTP, then watch ---"
BASE="http://127.0.0.1:$APP_PORT" PROJECT="$PROJECT" IID="$IID" MODEL="$MODEL" \
CODEHUB_BIN="$CODEHUB_BIN" UI_PORT="$UI_PORT" python3 - <<'PY'
import json, os, time, urllib.request
base=os.environ["BASE"]
def call(method, path, body=None):
    data=json.dumps(body).encode() if body is not None else None
    req=urllib.request.Request(base+path, data=data, method=method,
        headers={"content-type":"application/json"})
    with urllib.request.urlopen(req) as r:
        return json.load(r) if r.length!=0 else {}

SYSTEM=(
 "You are a senior Go / Kubernetes code reviewer for the CodeHub project "
 "example-org/k8s-extension. You drive the whole review yourself with the "
 "codehub CLI (v1.x, authenticated) via Bash. Do NOT git clone — codehub reads the PR remotely.\n"
 "For the PR you are given (project, iid, source_branch, target_branch, head_sha):\n"
 "1. Diff: `codehub pr diff <iid> -P <project> --no-pager`; fuller context: "
 "`codehub cat <source_branch>:<path> -P <project> --no-pager`.\n"
 "2. Review for real defects (correctness, goroutine leaks, nil/err, resource cleanup, CRD compat, security); cite file:line.\n"
 "3. Check prior comments: `codehub review comments list <iid> -P <project> --json`; never duplicate.\n"
 "4. Act (REAL writes): issues -> `codehub review comments add <iid> -P <project> --type Problem "
 "--file <path> --line <n> -m \"...\"` and STOP (leave open, do not approve). "
 "Clean/all resolved -> `codehub pr approve <iid> -P <project>` + a brief --type Comment summary.\n"
 "Run real commands, never fabricate. Be decisive. End with a 3-5 line Chinese summary + the codehub write commands you ran."
)

agent=call("POST","/v1/agents",{
    "name":"pr-reviewer","model":{"id":os.environ["MODEL"]},"system":SYSTEM,
    "metadata":{"shell_access":"true","runtime_timeout":"900s"}})
aid=agent["id"]; print("  agent:",aid)
env=call("POST","/v1/environments",{"name":"pr-review"}); eid=env["id"]; print("  env:",eid)

call("POST","/v1/eventbus/sources",{
    "name":"pr-source","type":"codehub-pr","project":os.environ["PROJECT"],
    "reviewer":"@me","interval":"30s","bin":os.environ["CODEHUB_BIN"],
    "allow_iids":[int(os.environ["IID"])]})
print("  source: pr-source (codehub-pr, allow",os.environ["IID"],")")
call("POST","/v1/eventbus/handlers",{
    "name":"pr-review","match":{"type":"pr"},
    "policy":{"kind":"keyed","agent_id":aid,"env_id":eid,
        "key_template":"{{.subject}}",
        "prompt_template":"Review PR {{.payload.iid}} now (source {{.payload.source_branch}} -> "
                          "target {{.payload.target_branch}}, head {{.payload.head_sha}}, "
                          "project {{.payload.project}}). Follow your standing review procedure."}})
print("  handler: pr-review (keyed by subject)")
print("  sources:",[s['name'] for s in call('GET','/v1/eventbus/sources')['data']],
      "handlers:",[h['name'] for h in call('GET','/v1/eventbus/handlers')['data']])

print("  waiting for the poller to create a session (watch http://127.0.0.1:%s) ..."%os.environ["UI_PORT"])
sid=None; t=time.time()+90
while time.time()<t:
    s=call("GET","/v1/sessions").get("data",[])
    if s: sid=s[0]["id"]; break
    time.sleep(2)
if not sid: print("  NO SESSION dispatched"); raise SystemExit(1)
print("  session:",sid)

seen=set(); deadline=time.time()+900
while time.time()<deadline:
    try: events=call("GET","/v1/sessions/%s/events"%sid).get("data",[])
    except Exception: time.sleep(2); continue
    done=False
    for e in events:
        if e["id"] in seen: continue
        seen.add(e["id"]); ty=e["type"]
        if ty=="user.message":        print("  → user:","".join(b.get("text","") for b in e.get("content",[]))[:240])
        elif ty=="agent.tool_use":    print("  · tool_use:",e.get("name"),json.dumps(e.get("input",{}))[:200])
        elif ty=="agent.tool_result": print("  · result:","".join(b.get("text","") for b in (e.get("content") or []))[:200].replace("\n","\\n"))
        elif ty=="agent.message":     print("  ← agent:","".join(b.get("text","") for b in e.get("content",[]))[:1000])
        elif ty=="session.status_idle": done=True
    if done: break
    time.sleep(2)
print("  --- turn complete ---")
PY
echo "logs kept at: $RUN"
