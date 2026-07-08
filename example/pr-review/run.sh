#!/usr/bin/env bash
# pr-review end-to-end: boot an ISOLATED, token-free ahsir scheduler +
# hetairoi with the built-in CodeHub PR poller, scoped to one PR (REVIEW_IID,
# default 3177). A matching review agent (Claude runtime, shell_access) reviews
# the PR over the codehub CLI and posts a comment / approve — REAL writes.
#
#   ./run.sh               # review PR 3177
#   REVIEW_IID=3179 ./run.sh
#
# Ports are deliberately in a high, isolated block (198xx) so they never collide
# with a persistent ahsir fleet on 9800/98xx. Cleanup only kills THIS run's own
# processes — never a broad pkill that could hit your standing agents.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
AHSIR=${AHSIR_DIR:-$REPO/../ahsir}

PROJECT=${REVIEW_PROJECT:-example-org/k8s-extension}
IID=${REVIEW_IID:-3177}
CODEHUB_BIN=${CMA_CODEHUB_BIN:-$HOME/.local/bin/codehub}
AHSIR_PORT=${AHSIR_PORT:-19800}     # isolated from the 9800 fleet
UI_PORT=${AHSIR_UI_PORT:-19801}
APP_PORT=${APP_PORT:-18790}
PR_LO=19811; PR_HI=19860            # test agent port_range (isolated)

export PATH="$HOME/.superset/bin:$HOME/.local/bin:$PATH"
export no_proxy=127.0.0.1,localhost NO_PROXY=127.0.0.1,localhost

command -v claude  >/dev/null || { echo "claude CLI not found on PATH"; exit 1; }
[ -x "$CODEHUB_BIN" ] || { echo "codehub not found at $CODEHUB_BIN"; exit 1; }
"$CODEHUB_BIN" auth whoami >/dev/null 2>&1 || { echo "codehub not logged in (run: codehub auth login)"; exit 1; }
[ -d "$AHSIR" ] || { echo "ahsir checkout not found at $AHSIR (set AHSIR_DIR)"; exit 1; }

# Pre-flight: never start on a port someone else holds (e.g. your fleet).
for port in $AHSIR_PORT $UI_PORT $APP_PORT; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "port $port already in use — pick another (AHSIR_PORT/AHSIR_UI_PORT/APP_PORT) or free it"; exit 1
  fi
done

RUN=$(mktemp -d)
echo "logs: $RUN"
cleanup() {
  echo "--- cleanup (only this run's processes) ---"
  [ -n "${UI_PID:-}" ]    && kill "$UI_PID"    2>/dev/null || true
  [ -n "${APP_PID:-}" ]   && kill "$APP_PID"   2>/dev/null || true
  [ -n "${SCHED_PID:-}" ] && kill "$SCHED_PID" 2>/dev/null || true
  # Kill ONLY agents registered to THIS test scheduler (scoped by its port),
  # never a broad ahsir-agent match that would hit a standing fleet.
  pkill -f "registry http://127.0.0.1:$AHSIR_PORT" 2>/dev/null || true
  pkill -f "exe/pr-review" 2>/dev/null || true   # the go-run child binary outlives its parent
}
trap cleanup EXIT

cat > "$RUN/ahsir.yaml" <<YAML
registry: { host: "127.0.0.1", port: $AHSIR_PORT, heartbeat_interval: 10s, heartbeat_timeout: 30s }
port_range: { start: $PR_LO, end: $PR_HI }
timeouts: { chat: 15m, task_status: 30s }
YAML

echo "--- 1. build + start ahsir scheduler ($AHSIR_PORT) ---"
( cd "$AHSIR" && GO111MODULE=on go build -o bin/ahsir ./cmd/ahsir && GO111MODULE=on go build -o bin/ahsir-agent ./cmd/ahsir-agent )
# `ahsir start <config>` resolves its admin token as: AHSIR_ADMIN_TOKEN env →
# existing admin-token file → auto-generate. We export one shared value so the
# scheduler, the UI, and hetairoi all agree without anyone reading a file.
export AHSIR_ADMIN_TOKEN="${AHSIR_ADMIN_TOKEN:-local-token}"
"$AHSIR/bin/ahsir" start "$RUN/ahsir.yaml" > "$RUN/ahsir.log" 2>&1 &
SCHED_PID=$!
up=0
for _ in $(seq 1 30); do
  kill -0 "$SCHED_PID" 2>/dev/null || { echo "scheduler died on boot:"; cat "$RUN/ahsir.log"; exit 1; }
  curl -sf "http://127.0.0.1:$AHSIR_PORT/agents" >/dev/null 2>&1 && { up=1; break; }
  sleep 0.5
done
[ "$up" = 1 ] || { echo "scheduler not reachable on $AHSIR_PORT:"; cat "$RUN/ahsir.log"; exit 1; }

echo "--- 1b. start ahsir UI ($UI_PORT) for live monitoring ---"
"$AHSIR/bin/ahsir" ui --addr "127.0.0.1:$UI_PORT" --scheduler "http://127.0.0.1:$AHSIR_PORT" > "$RUN/ui.log" 2>&1 &
UI_PID=$!
echo ">>> ahsir UI:  http://127.0.0.1:$UI_PORT   (scheduler $AHSIR_PORT)"

echo "--- 2. start pr-review ($APP_PORT), scoped to PR $IID ---"
GO111MODULE=on \
  CMA_LISTEN=127.0.0.1:$APP_PORT CMA_AHSIR_URL=http://127.0.0.1:$AHSIR_PORT \
  CMA_AHSIR_ADMIN_TOKEN="$AHSIR_ADMIN_TOKEN" \
  CMA_RUNTIME_PROVIDER=anthropic \
  CMA_STATE_FILE="$RUN/state.json" \
  CMA_CODEHUB_BIN="$CODEHUB_BIN" CMA_CODEHUB_PROJECT="$PROJECT" CMA_CODEHUB_REVIEWER=@me \
  CMA_CODEHUB_INTERVAL=30s CMA_CODEHUB_ALLOW_IIDS="$IID" \
  go run "$REPO/cmd/pr-review" > "$RUN/app.log" 2>&1 &
APP_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$APP_PID" 2>/dev/null || { echo "pr-review died:"; cat "$RUN/app.log"; exit 1; }
  curl -sf "http://127.0.0.1:$APP_PORT/v1/agents" >/dev/null 2>&1 && break
  sleep 0.5
done

echo "--- 3. wait for the poller to dispatch PR $IID to a session ---"
SID=""
for _ in $(seq 1 40); do
  SID=$(grep -oE 'session=[a-zA-Z0-9_]+' "$RUN/app.log" | head -1 | cut -d= -f2 || true)
  [ -n "$SID" ] && break
  grep -q "err=register" "$RUN/app.log" && { echo "register failed:"; grep "err=" "$RUN/app.log" | head; exit 1; }
  sleep 1
done
[ -n "$SID" ] || { echo "no dispatch yet; app.log:"; tail -20 "$RUN/app.log"; exit 1; }
echo "  session: $SID   (watch live at http://127.0.0.1:$UI_PORT)"

echo "--- 4. watch the agent review (tool_use / tool_result / agent.message) ---"
APP_PORT=$APP_PORT SID="$SID" python3 - <<'PY'
import json, os, time, urllib.request
port=os.environ["APP_PORT"]; sid=os.environ["SID"]
seen=set(); deadline=time.time()+900
while time.time()<deadline:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/sessions/{sid}/events") as r:
            events=json.load(r)["data"]
    except Exception:
        time.sleep(2); continue
    done=False
    for e in events:
        if e["id"] in seen: continue
        seen.add(e["id"]); t=e["type"]
        if t=="user.message":        print("  → user:", "".join(b.get("text","") for b in e.get("content",[]))[:300])
        elif t=="agent.thinking":    print("  · thinking:", (e.get("text") or "")[:160].replace("\n","\\n"))
        elif t=="agent.tool_use":    print("  · tool_use:", e.get("name"), json.dumps(e.get("input",{}))[:200])
        elif t=="agent.tool_result": print("  · result:", "".join(b.get("text","") for b in (e.get("content") or []))[:220].replace("\n","\\n"))
        elif t=="agent.message":     print("  ← agent:", "".join(b.get("text","") for b in e.get("content",[]))[:1200])
        elif t=="session.status_idle": done=True
    if done: break
    time.sleep(2)
print("  --- turn complete ---")
PY
echo "logs kept at: $RUN  (ahsir.log ui.log app.log state.json)"
