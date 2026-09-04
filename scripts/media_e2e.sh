#!/usr/bin/env bash
# Drive the media subsystem end to end, against a real ffmpeg and a real Postgres.
#
# Everything this asserts is invisible to a unit test: whether the argv the
# builders produce is argv ffmpeg accepts, whether the package it writes is one a
# player can read, and whether the address handed back actually serves bytes. The
# encode is the product, so the encode is what runs here.
#
#     ./scripts/media_e2e.sh ./media/aura-media "postgres://postgres:media@localhost:5432/postgres"
#
# Needs: ffmpeg and ffprobe on PATH, curl, and a reachable Postgres.
set -euo pipefail

BINARY="${1:-./media/aura-media}"
DSN="${2:-${AURA_MEDIA_TEST_DSN:-}}"
PORT="${MEDIA_PORT:-9390}"
WORK="$(mktemp -d)"
TOKEN="e2e-$(head -c 18 /dev/urandom | base64 | tr -d '=+/')"
NODE_PID=""

cleanup() {
  [ -n "$NODE_PID" ] && kill "$NODE_PID" 2>/dev/null || true
  sleep 1
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1" >&2; [ -f "$WORK/media.log" ] && tail -30 "$WORK/media.log" >&2; exit 1; }

[ -n "$DSN" ] || fail "no DSN: pass one as \$2 or set AURA_MEDIA_TEST_DSN"
command -v ffmpeg >/dev/null || fail "ffmpeg is not on PATH"

api() {
  local method="$1" path="$2"; shift 2
  curl -sS -X "$method" -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT$path" "$@"
}
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }

# A source with both streams and a length that varies per run, so a re-run
# encodes rather than deduplicating against the last one's digest.
DURATION=$(( 8 + RANDOM % 7 ))
ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "testsrc=size=1280x720:rate=25:duration=$DURATION" \
  -f lavfi -i "sine=frequency=440:duration=$DURATION" \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -shortest "$WORK/source.mp4"
pass "made a ${DURATION}s 720p source with audio"

AURA_MEDIA_TOKEN="$TOKEN" "$BINARY" serve \
  --dsn "$DSN" --bind "127.0.0.1:$PORT" --data "$WORK/data" --workers 2 \
  > "$WORK/media.log" 2>&1 &
NODE_PID=$!
for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null || fail "the node never became healthy"
pass "node is up"

[ "$(code "http://127.0.0.1:$PORT/readyz")" = "200" ] || fail "readiness is not open, or the node is not ready"
[ "$(code "http://127.0.0.1:$PORT/v1/queue")" = "401" ] || fail "the control surface answered without a token"
pass "readiness is open and the control surface is not"

# Keyed on the content, which is what an idempotency key is for. A key that
# collides across runs would hand this run the previous one's asset — correct
# idempotency, and a confusing way to fail an end-to-end test.
KEY="e2e-$(sha256sum "$WORK/source.mp4" | cut -c1-32)"
RESPONSE=$(api POST /v1/assets -H "Content-Type: video/mp4" -H "Idempotency-Key: $KEY" --data-binary "@$WORK/source.mp4")
ASSET=$(printf '%s' "$RESPONSE" | field id)
[ -n "$ASSET" ] || fail "the upload returned no asset: $RESPONSE"
pass "upload accepted as $ASSET"

STATE=""
for _ in $(seq 1 180); do
  BODY=$(api GET "/v1/assets/$ASSET")
  STATE=$(printf '%s' "$BODY" | field state)
  [ "$STATE" = "ready" ] && break
  [ "$STATE" = "failed" ] && fail "the encode failed: $BODY"
  sleep 1
done
[ "$STATE" = "ready" ] || fail "the asset never became ready (last state: $STATE)"
pass "encoded and packaged"

ADDRESS=$(printf '%s' "$BODY" | field address)
[ -n "$ADDRESS" ] || fail "a ready asset has no address: $BODY"
MASTER=$(curl -sf "http://127.0.0.1:$PORT$ADDRESS") || fail "the address does not serve: $ADDRESS"
printf '%s' "$MASTER" | grep -q "#EXTM3U" || fail "the address does not serve a playlist"
pass "the master playlist serves with no token, at $ADDRESS"

# Three rungs for a 720p source, and the resolutions the record claims must be
# the resolutions the playlist claims — a player reads the playlist.
for rung in 720 480 360; do
  printf '%s' "$MASTER" | grep -q "${rung}p/index.m3u8" || fail "no ${rung}p rung in the master playlist"
done
printf '%s' "$MASTER" | grep -q "RESOLUTION=854x480" || fail "the 480p rung is not 854 wide; scale=-2 rounds to the nearest even number"
printf '%s' "$BODY" | grep -q '"width":854' || fail "the asset record disagrees with the playlist about the 480p width"
pass "three rungs, and the record agrees with the playlist"

BASE=$(dirname "$ADDRESS")
[ "$(code "http://127.0.0.1:$PORT$BASE/720p/seg_00000.m4s")" = "200" ] || fail "the first segment does not serve"
pass "segments serve"

# The source is the clip before anything de-identified it. It is in the same
# store and it must not be reachable over the public tree, with or without a token.
[ "$(code "http://127.0.0.1:$PORT/media/src/$ASSET")" = "404" ] || fail "a source was served over the public tree"
[ "$(code -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/media/src/$ASSET")" = "404" ] || fail "a source was served to a token holder over the public tree"
pass "sources are not public"

printf '%s' "$BODY" | grep -q '"frames":\[' || fail "no frames were sampled; every AI capability downstream needs them"
printf '%s' "$BODY" | grep -q '"poster":' || fail "no poster was taken"
pass "frames and poster came out of the same decode"

# The whole point of the package: something can play it.
ffmpeg -hide_banner -loglevel error -y -i "http://127.0.0.1:$PORT$ADDRESS" -t 3 -f null - 2>"$WORK/play.log" \
  || fail "ffmpeg could not play the packaged HLS: $(tail -3 "$WORK/play.log")"
pass "ffmpeg plays the packaged HLS back"

AGAIN=$(api POST /v1/assets -H "Content-Type: video/mp4" --data-binary "@$WORK/source.mp4")
printf '%s' "$AGAIN" | grep -q '"deduplicated":true' || fail "the same bytes were accepted for a second encode"
[ "$(printf '%s' "$AGAIN" | field id)" = "$ASSET" ] || fail "the duplicate upload returned a different asset"
pass "identical bytes deduplicate instead of encoding twice"

# SIGTERM is what `docker stop` and a pod deletion send, so it runs on every
# deploy: it must drain rather than be killed.
kill -TERM "$NODE_PID"
for _ in $(seq 1 40); do
  kill -0 "$NODE_PID" 2>/dev/null || break
  sleep 0.5
done
if kill -0 "$NODE_PID" 2>/dev/null; then
  kill -9 "$NODE_PID"
  fail "the node ignored SIGTERM and had to be killed"
fi
grep -q "stopped cleanly" "$WORK/media.log" || fail "no orderly shutdown; the drain did not run"
NODE_PID=""
pass "SIGTERM drains rather than kills"

printf '\nmedia end to end: everything above passed.\n'
