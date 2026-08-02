#!/usr/bin/env bash
# Check that a node refuses what it is supposed to refuse.
#
# Every assertion here is something the node used to allow, and each is a claim
# about what a person on the network can do rather than about what a function
# returns — so they run against a real binary over a real socket, with the
# default flags. Testing the hardened path while shipping a permissive default
# would prove nothing.
#
#     ./scripts/adversarial.sh ./kernel/aura
#
set -euo pipefail

AURA="${1:-./kernel/aura}"
WORK="$(mktemp -d)"
PORT_DEFAULT=9081
PORT_POLICY=9082
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  # Give the nodes a moment to release their SQLite files before removing the
  # tree, or the cleanup noisily fails on platforms that lock open files.
  sleep 1
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

pass() { printf '  ok   %s\n' "$1"; }
skip() { printf '  --   %s (skipped: %s)\n' "$1" "$2"; }
fail() { printf '  FAIL %s\n' "$1" >&2; exit 1; }

# posix_modes reports whether this filesystem actually carries POSIX mode bits.
# Git Bash on Windows answers 644 for every file whatever it was created with,
# so asking there produces a failure that says nothing about the code.
posix_modes() {
  local probe="$WORK/.mode-probe"
  : > "$probe"
  chmod 600 "$probe" 2>/dev/null || return 1
  [ "$(stat -c '%a' "$probe" 2>/dev/null)" = "600" ]
}

wait_healthy() {
  local port="$1"
  for _ in $(seq 1 30); do
    if curl -fsS "http://localhost:${port}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  fail "node on :${port} never became healthy"
}

status() {
  curl -s -o /dev/null -w '%{http_code}' "$@"
}

# ── A default node ────────────────────────────────────────────────
echo "a node started with no flags:"

DATA_DEFAULT="$WORK/default"
"$AURA" up --port "$PORT_DEFAULT" --data "$DATA_DEFAULT" > "$WORK/banner.txt" 2>&1 &
PIDS+=($!)
wait_healthy "$PORT_DEFAULT"

# The catalog names every effect this node can produce, which is the most
# useful thing an attacker could read.
code="$(status "http://localhost:${PORT_DEFAULT}/v1/skills")"
[ "$code" = "401" ] || fail "/v1/skills answered $code without a token"
pass "refuses the control surface without a token"

TOKEN="$(tr -d '\r\n' < "$DATA_DEFAULT/node.token")"
code="$(status -H "Authorization: Bearer $TOKEN" "http://localhost:${PORT_DEFAULT}/v1/skills")"
[ "$code" = "200" ] || fail "the node rejected the token it generated ($code)"
pass "accepts the token it generated"

code="$(status -H "Authorization: Bearer not-the-token" "http://localhost:${PORT_DEFAULT}/v1/skills")"
[ "$code" = "401" ] || fail "a wrong token answered $code"
pass "refuses a wrong token"

# An orchestrator has no credential and must still be able to ask if the
# process is up.
code="$(status "http://localhost:${PORT_DEFAULT}/healthz")"
[ "$code" = "200" ] || fail "/healthz answered $code; liveness probes hold no token"
pass "leaves /healthz open for liveness probes"

if posix_modes; then
  perms="$(stat -c '%a' "$DATA_DEFAULT/node.token")"
  [ "$perms" = "600" ] || fail "node.token is mode $perms; a credential every account can read is not one"
  pass "writes the token owner-only"
else
  skip "writes the token owner-only" "no POSIX mode bits on this filesystem"
fi

# The old default served a laptop in a café to the café.
if command -v ss >/dev/null 2>&1; then
  if ss -ltn 2>/dev/null | grep -qE "0\.0\.0\.0:${PORT_DEFAULT}\b|\[::\]:${PORT_DEFAULT}\b"; then
    ss -ltn
    fail "the default binding is public"
  fi
  pass "binds loopback rather than every interface"
fi

grep -q 'policy' "$WORK/banner.txt" || fail "the banner does not say which policy is in force"
pass "reports its policy at startup"

# ── A node under a policy ─────────────────────────────────────────
echo
echo "a node started with a policy:"

cat > "$WORK/aura.policy.yaml" <<'POLICY'
policy: 1
default_effect: gate
rules:
  - match: "motor.payments.*"
    decision: deny
    reason: "no automated payments on this node"
POLICY

DATA_POLICY="$WORK/policy"
"$AURA" up --port "$PORT_POLICY" --data "$DATA_POLICY" \
  --policy "$WORK/aura.policy.yaml" > "$WORK/policy-banner.txt" 2>&1 &
PIDS+=($!)
wait_healthy "$PORT_POLICY"

grep -q 'sha256:' "$WORK/policy-banner.txt" \
  || { cat "$WORK/policy-banner.txt"; fail "the node does not report the hash of the policy it loaded"; }
pass "reports the hash of the policy it loaded"

POLICY_TOKEN="$(tr -d '\r\n' < "$DATA_POLICY/node.token")"

# A graph that waives the gate on an effect is still a well-formed document, so
# it registers. Registration is not the enforcement point — instantiation is,
# which is what the executor's tests cover. What matters here is that the
# waiver reaches a node that will not honour it.
code="$(status -X POST \
  -H "Authorization: Bearer $POLICY_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"ir":"1","graph_id":"waiver","origin":{"kind":"declared"},"nodes":[{"ref":"w","resolve":"motor.payments.send"}],"edges":[{"from":"client.text_out","to":"w.text_in","gate":"none"}]}' \
  "http://localhost:${PORT_POLICY}/v1/graphs")"
[ "$code" = "201" ] || fail "registering a well-formed graph answered $code"
pass "accepts a gate:none graph as a document"

# Opening a session on it must fail: the capability is denied outright, and no
# graph may talk its way past a deny.
body="$(curl -s -i -N \
  -H "Authorization: Bearer $POLICY_TOKEN" \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  "http://localhost:${PORT_POLICY}/v1/stream?graph=waiver" 2>&1 || true)"
if printf '%s' "$body" | grep -q '101 Switching Protocols'; then
  # The socket opened, so the refusal has to arrive as an error envelope
  # instead — either shape is a refusal, neither is execution.
  printf '%s' "$body" | grep -q '"kind":"error"' \
    || fail "a session opened on a graph whose capability is denied"
fi
pass "refuses to run a graph whose capability is denied"

echo
echo "all adversarial checks passed"
