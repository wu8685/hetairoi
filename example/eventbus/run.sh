#!/usr/bin/env bash
# End-to-end event-bus demo: boot ahsir + the eventbus-example service, POST an
# alert to the webhook, and watch the agent investigate with real shell commands.
#
# Requires: Go, a checked-out ../ahsir, and a provider key. Defaults to DeepSeek.
#   DEEPSEEK_API_KEY=sk-... ./run.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
AHSIR=${AHSIR_DIR:-$REPO/../ahsir}
PROVIDER=${EXAMPLE_PROVIDER:-deepseek}
APIKEY=${EXAMPLE_API_KEY:-${DEEPSEEK_API_KEY:-}}
MODEL=${EXAMPLE_MODEL:-deepseek-chat}
TOKEN=eventbus-example-token
AHSIR_PORT=9810
APP_PORT=8790

# Bypass any local/corporate proxy for loopback (curl + urllib honor no_proxy).
export no_proxy=127.0.0.1,localhost NO_PROXY=127.0.0.1,localhost

[ -n "$APIKEY" ] || { echo "set DEEPSEEK_API_KEY (or EXAMPLE_API_KEY)"; exit 1; }
[ -d "$AHSIR" ] || { echo "ahsir checkout not found at $AHSIR (set AHSIR_DIR)"; exit 1; }

RUN=$(mktemp -d)
cleanup() {
  echo "--- cleanup ---"
  [ -n "${APP_PID:-}" ] && kill "$APP_PID" 2>/dev/null || true
  [ -n "${SCHED_PID:-}" ] && kill "$SCHED_PID" 2>/dev/null || true
  pkill -f "$AHSIR/bin/ahsir-agent" 2>/dev/null || true
}
trap cleanup EXIT

cat > "$RUN/ahsir.yaml" <<YAML
registry: { host: "127.0.0.1", port: $AHSIR_PORT, heartbeat_interval: 10s, heartbeat_timeout: 30s }
port_range: { start: 9811, end: 9899 }
timeouts: { chat: 10m, task_status: 30s }
YAML

echo "--- 1. build + start ahsir scheduler ($AHSIR_PORT) ---"
( cd "$AHSIR" && GO111MODULE=on go build -o bin/ahsir ./cmd/ahsir && GO111MODULE=on go build -o bin/ahsir-agent ./cmd/ahsir-agent )
AHSIR_ADMIN_TOKEN=$TOKEN "$AHSIR/bin/ahsir" start "$RUN/ahsir.yaml" > "$RUN/ahsir.log" 2>&1 &
SCHED_PID=$!
for _ in $(seq 1 30); do curl -sf "http://127.0.0.1:$AHSIR_PORT/agents" >/dev/null 2>&1 && break; sleep 0.5; done

echo "--- 2. start eventbus-example ($APP_PORT) ---"
# GO111MODULE=on is required: this module lives under GOPATH/src and the global
# default is off; GOPATH mode ignores the go directive and breaks the 1.22
# method-based mux patterns (every route 404s).
GO111MODULE=on \
  CMA_LISTEN=127.0.0.1:$APP_PORT CMA_AHSIR_URL=http://127.0.0.1:$AHSIR_PORT CMA_AHSIR_ADMIN_TOKEN=$TOKEN \
  CMA_RUNTIME_PROVIDER=$PROVIDER CMA_RUNTIME_API_KEY="$APIKEY" EXAMPLE_MODEL="$MODEL" \
  CMA_STATE_FILE="$RUN/state.json" \
  go run "$REPO/cmd/eventbus-example" > "$RUN/example.log" 2>&1 &
APP_PID=$!
for _ in $(seq 1 30); do curl -sf "http://127.0.0.1:$APP_PORT/v1/agents" >/dev/null 2>&1 && break; sleep 0.5; done

echo "--- 3. POST an alert to the webhook ---"
ALERT='{"id":"alert-1","type":"alert","subject":"disk-pressure /tmp","payload":{"usage":"92%","mount":"/tmp"}}'
echo "  $ALERT"
RESP=$(curl -s -X POST "http://127.0.0.1:$APP_PORT/eventbus/events" -H 'content-type: application/json' -d "$ALERT")
echo "  webhook response: $RESP"

echo "--- 4. watch the agent work (session event log) ---"
APP_PORT=$APP_PORT RESP="$RESP" python3 - <<'PY'
import json, os, time, urllib.request
port = os.environ["APP_PORT"]; resp = json.loads(os.environ["RESP"])
sid = resp["results"][0]["session_id"]
print(f"  session: {sid}")
seen = set(); deadline = time.time() + 180
while time.time() < deadline:
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/v1/sessions/{sid}/events") as r:
        events = json.load(r)["data"]
    done = False
    for e in events:
        if e["id"] in seen: continue
        seen.add(e["id"]); t = e["type"]
        if t == "user.message":  print("  → user:", "".join(b["text"] for b in e.get("content",[]))[:200])
        elif t == "agent.tool_use":  print("  · tool_use:", e.get("name"), json.dumps(e.get("input",{}))[:120])
        elif t == "agent.tool_result":  print("  · tool_result:", "".join(b.get("text","") for b in (e.get("content") or []))[:160].replace("\n","\\n"))
        elif t == "agent.message":  print("  ← agent:", "".join(b["text"] for b in e.get("content",[]))[:600])
        elif t == "session.status_idle":  done = True
    if done: break
    time.sleep(1.5)
print("  --- turn complete ---")
PY
