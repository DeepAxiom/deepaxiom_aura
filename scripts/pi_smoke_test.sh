#!/usr/bin/env bash
# Closes Beta exit criterion #9: the full flow, on real arm64 hardware.
# Nothing here is new infrastructure — it's the same conformance suite and
# adversarial checks ci.yml already runs on amd64 on every push, pointed at
# a binary actually running on the target hardware, so passing here means
# "arm64" stops being an untested claim.
#
# Run on the Pi itself (or any Linux arm64 box), from a checkout of this repo:
#
#     ./scripts/pi_smoke_test.sh [path-to-aura-binary]
#
# If no binary path is given, builds one from source (needs Go 1.25+ on the
# device — cross-compiling elsewhere and copying the binary over is fine
# too, just pass its path as the argument).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AURA="${1:-}"
PORT=9083
WORK="$(mktemp -d)"
PIDS=()

pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1" >&2; exit 1; }

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  sleep 1
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

# 1. This has to actually be arm64, or the rest of this script "passing"
# would just mean amd64 works again, which is already covered by ci.yml.
arch="$(uname -m)"
case "$arch" in
  aarch64|arm64) pass "host architecture is $arch" ;;
  *)
    fail "host architecture is '$arch', not arm64 — run this ON the Pi, not against it"
    ;;
esac

if [ -z "$AURA" ]; then
  echo "no binary given, building from source..."
  ( cd "$root/kernel" && go build -o "$WORK/aura" ./cmd/aura )
  AURA="$WORK/aura"
fi
[ -x "$AURA" ] || fail "'$AURA' is not an executable file"

binary_arch="$(file -b "$AURA" 2>/dev/null || true)"
case "$binary_arch" in
  *aarch64*|*ARM\ aarch64*|*arm64*) pass "binary is arm64 ($binary_arch)" ;;
  *)
    echo "  --   could not confirm binary architecture from 'file' output: $binary_arch" >&2
    echo "        (continuing — the exec below will fail outright if it's wrong)" >&2
    ;;
esac

# 2. Same startup/health-check loop as ci.yml's conformance job.
"$AURA" up --port "$PORT" --no-auth --data "$WORK/data" &
PIDS+=("$!")

healthy=0
for i in $(seq 1 30); do
  if curl -fsS "http://localhost:$PORT/healthz" >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 1
done
[ "$healthy" = "1" ] || fail "node never became healthy on port $PORT"
pass "node is up and healthy on arm64"

# 3. The real C1/C2/C3 conformance suite, over the wire, against this node.
python3 "$root/spec/conformance/runner.py" --port "$PORT"
pass "conformance suite (C1/C2/C3)"

# 4. C4: drive real effects through the gate, seal them, confirm `aura
# verify` and GET /v1/ledger/verify agree the chain is sound — then
# deliberately tamper with kernel.db and confirm both catch it. This is the
# only ledger-soundness check in this script on purpose: it already proves
# both the sound and the tampered case, so a bare `aura verify` afterward
# would be redundant, and wrong besides — this step leaves the ledger
# intentionally broken as part of proving tampering is detected.
python3 "$root/scripts/ledger_adversarial.py" --port "$PORT" \
  --data "$WORK/data" --aura-binary "$AURA"
pass "ledger adversarial (C4) — sound before tampering, caught after"

# 5. Default-node hardening — same script as ci.yml's adversarial job, on
# its own ports, so it can run right after the checks above.
"$root/scripts/adversarial.sh" "$AURA"
pass "adversarial (default node refuses what it should)"

echo
echo "arm64 smoke test: all checks passed."
