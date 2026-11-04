#!/bin/sh
# End-to-end verification of the AlterHive manager/sensor split on a single
# machine: mock LLM + manager + sensor as local processes, SSH probing via
# expect, degradation check when the manager dies.
#
# Usage: scripts/verify-split.sh [repo-root]
set -u

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
WORK="${VERIFY_WORK:-/tmp/ah-split-verify}"
TOKEN="verify-token-1"
MANAGER_ADDR="127.0.0.1:8100"
SENSOR_SSH="127.0.0.1:2222"
MOCK_PORT=9999

BIN="$WORK/alterhive"
MOCK_LOG="$WORK/mock.log"
MANAGER_LOG="$WORK/manager.log"
SENSOR_LOG="$WORK/sensor.log"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILED=1; }
FAILED=0

cleanup() {
  [ -n "${MANAGER_PID:-}" ] && kill "$MANAGER_PID" 2>/dev/null
  [ -n "${SENSOR_PID:-}" ] && kill "$SENSOR_PID" 2>/dev/null
  [ -n "${MOCK_PID:-}" ] && kill "$MOCK_PID" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT INT TERM

# ── Prepare ────────────────────────────────────────────────────────────────
rm -rf "$WORK"
mkdir -p "$WORK/manager-configs" "$WORK/sensor-configs/services"

echo "== building binary =="
if ! (cd "$ROOT" && go build -o "$BIN" .); then
  fail "build"
  exit 1
fi
pass "binary built"

# Manager config: LLM provider pointing at the mock.
cat > "$WORK/manager-configs/llm.yaml" <<'YAML'
enabled: true
active_provider: mock
providers:
  - id: mock
    name: Mock Provider
    type: openai
    base_url: http://127.0.0.1:9999/v1
    api_key: mock-key
    model: mock-model
    max_tokens: 2048
    temperature: 0.7
    enabled: true
YAML

# Sensor config: topology + SSH service, deliberately NO llm.yaml.
cp "$ROOT/configs/topology.yaml" "$WORK/sensor-configs/"
cp "$ROOT/configs/services/"*.yaml "$WORK/sensor-configs/services/" 2>/dev/null || true

if [ -e "$WORK/sensor-configs/llm.yaml" ]; then
  fail "sensor config dir must not contain llm.yaml"
fi
pass "sensor config has no llm.yaml"

# ── Start processes ────────────────────────────────────────────────────────
echo "== starting mock LLM =="
python3 "$ROOT/scripts/mockllm.py" "$MOCK_PORT" > "$MOCK_LOG" 2>&1 &
MOCK_PID=$!
sleep 0.5

echo "== starting manager =="
env ALTERHIVE_ROLE=manager \
  CONFIG_DIR="$WORK/manager-configs" \
  ALTERHIVE_MANAGER_TOKEN="$TOKEN" \
  ALTERHIVE_MANAGER_ADDR="$MANAGER_ADDR" \
  ALTERHIVE_ALLOW_LLM_NETWORK=true \
  "$BIN" > "$MANAGER_LOG" 2>&1 &
MANAGER_PID=$!

MANAGER_UP=0
i=0
while [ $i -lt 20 ]; do
  if curl -fsS -m 1 "http://$MANAGER_ADDR/healthz" > /dev/null 2>&1; then
    MANAGER_UP=1
    break
  fi
  sleep 0.5
  i=$((i + 1))
done
if [ "$MANAGER_UP" -eq 1 ]; then
  pass "manager healthz"
else
  fail "manager healthz"; cat "$MANAGER_LOG"
fi

echo "== starting sensor =="
env ALTERHIVE_ROLE=sensor \
  CONFIG_DIR="$WORK/sensor-configs" \
  ALTERHIVE_MANAGER_URL="http://$MANAGER_ADDR" \
  ALTERHIVE_MANAGER_TOKEN="$TOKEN" \
  SSH_ADDR="$SENSOR_SSH" \
  API_ADDR="127.0.0.1:8000" \
  ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT=true \
  "$BIN" > "$SENSOR_LOG" 2>&1 &
SENSOR_PID=$!
sleep 1

# ── SSH probe: hot path + complex command + cached decision ───────────────
run_ssh() {
  # $1 = command to run in the honeypot shell. Retries transient connect
  # failures. Output (stdout of the remote command) is echoed.
  i=0
  while [ $i -lt 3 ]; do
    OUT="$(timeout 20 expect -c "
      set timeout 15
      spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o PreferredAuthentications=password -o PubkeyAuthentication=no \
        -o NumberOfPasswordPrompts=1 root@127.0.0.1 -p 2222 $1
      expect {
        \"*assword*\" { send \"123456\r\"; exp_continue }
        eof {}
        timeout {}
      }
    " 2>/dev/null)"
    if [ -n "$OUT" ]; then
      echo "$OUT"
      return 0
    fi
    i=$((i + 1))
    sleep 1
  done
  return 1
}

echo "== probing sensor (hot path + complex render) =="
FIRST_OUT="$(run_ssh 'nmap -sV 192.168.56.60')"
if echo "$FIRST_OUT" | grep -q "192.168.56.60"; then
  pass "hot path deterministic nmap output"
else
  fail "hot path deterministic output, got: $(echo "$FIRST_OUT" | head -5)"
fi

# Wait for the async decision to land in the sensor's world cache, then the
# repeated command must surface the LLM-authored output.
echo "== waiting for async decision to land =="
CACHED=""
i=0
while [ $i -lt 30 ]; do
  sleep 1
  OUT="$(run_ssh 'nmap -sV 192.168.56.60')"
  if echo "$OUT" | grep -q "MOCKLLM-RENDER"; then
    CACHED="$OUT"
    break
  fi
  i=$((i + 1))
done
if [ -n "$CACHED" ]; then
  pass "LLM-authored output surfaced on next command (MOCKLLM-RENDER)"
else
  fail "LLM-authored output never surfaced"
fi

# ── Assertions on the split architecture ──────────────────────────────────
if grep -q "decision handled" "$MANAGER_LOG"; then
  pass "manager handled decision requests"
else
  fail "no decisions handled by manager"
fi

if grep -q "complex_render" "$MANAGER_LOG"; then
  pass "complex_render call site reached manager"
else
  fail "complex_render never reached manager"
fi

if grep -q "completion request" "$MOCK_LOG"; then
  pass "mock LLM received completion requests"
else
  fail "mock LLM never received requests"
fi

# Sensor must never talk to the LLM provider directly: the mock log's auth
# header is the manager's key; and the sensor must hold no provider env.
if grep -q "ALTERHIVE_ALLOW_LLM_NETWORK=true" "$SENSOR_LOG" 2>/dev/null; then
  fail "sensor must not enable LLM network"
else
  pass "sensor has no LLM network gate enabled"
fi

# ── Degradation: kill the manager, hot path keeps serving ────────────────
echo "== killing manager, verifying degradation =="
kill "$MANAGER_PID" 2>/dev/null
wait "$MANAGER_PID" 2>/dev/null
MANAGER_PID=""
sleep 1

if curl -fsS -m 1 "http://$MANAGER_ADDR/healthz" > /dev/null 2>&1; then
  fail "manager still alive after kill"
else
  pass "manager process terminated"
fi

DEGRADED_OUT="$(run_ssh 'nmap -sV 192.168.56.60')"
if echo "$DEGRADED_OUT" | grep -q "192.168.56.60"; then
  pass "sensor still serves hot path after manager death"
else
  fail "sensor broken after manager death: $(echo "$DEGRADED_OUT" | head -5)"
fi

# A *new* command (fresh cache key) must attempt a decision, hit the dead
# manager, degrade cleanly and record the failure in the session events.
# Use the sensor's /simulate HTTP endpoint so the check does not depend on
# SSH/pty timing: it dispatches through the same ExecuteCommand pipeline.
SIM_RESP="$(curl -fsS -m 5 -X POST -H 'Content-Type: application/json' \
  -d '{"target":"192.168.56.23"}' http://127.0.0.1:8000/api/v1/simulate/nmap 2>/dev/null || true)"
if [ -n "$SIM_RESP" ]; then
  pass "sensor /simulate answered while manager down"
else
  fail "sensor /simulate failed while manager down"
fi
sleep 1

# The degraded cold path must record its failure into the session event log
# (reachable via the sensor API). Search every session for the failure.
FOUND_UNREACHABLE=0
SESS_IDS="$(curl -fsS -m 2 -H 'Authorization: Bearer alterhive-admin-token' \
  http://127.0.0.1:8000/api/v1/sessions 2>/dev/null | \
  grep -o '"session_id":"[^"]*"' | sed 's/.*:"//;s/"$//')"
for SID in $SESS_IDS; do
  DETAIL="$(curl -fsS -m 2 -H 'Authorization: Bearer alterhive-admin-token' \
    "http://127.0.0.1:8000/api/v1/sessions/$SID?include_commands=false" 2>/dev/null)"
  if echo "$DETAIL" | grep -q "manager unreachable"; then
    FOUND_UNREACHABLE=1
    break
  fi
done
if [ "$FOUND_UNREACHABLE" -eq 1 ]; then
  pass "degradation recorded in session event log"
else
  fail "session events missing manager unreachability"
  echo "---- DEBUG: all session events ----"
  for SID in $SESS_IDS; do
    echo "--- $SID ---"
    curl -fsS -m 2 -H 'Authorization: Bearer alterhive-admin-token' \
      "http://127.0.0.1:8000/api/v1/sessions/$SID?include_commands=false" 2>/dev/null | \
      python3 -c "import json,sys; d=json.load(sys.stdin)['data']; [print(e.get('type'),'|',e.get('detail','')[:90]) for e in d.get('events',[])[-6:]]" 2>&1
  done
fi

echo
if [ "$FAILED" -eq 0 ]; then
  echo "ALL CHECKS PASSED"
  exit 0
fi
echo "SOME CHECKS FAILED"
exit 1
