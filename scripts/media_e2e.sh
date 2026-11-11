#!/usr/bin/env bash
# Drive the media subsystem end to end, against a real ffmpeg and a real Postgres.
#
# Everything this asserts is invisible to a unit test: whether the argv the
# builders produce is argv ffmpeg accepts, whether the package it writes is one a
# player can read, and whether the address handed back actually serves bytes. The
# encode is the product, so the encode is what runs here.
#
# Two halves. The first uploads a file and watches it become playable before the
# encode has finished, then complete. The second pushes a live stream over RTMP,
# watches the playlist advance while it is live, drops and reconnects the
# broadcaster, and checks that what is left when it stops is an ordinary VOD.
#
#     ./scripts/media_e2e.sh ./media/aura-media "postgres://postgres:media@localhost:5432/postgres"
#
# Needs: ffmpeg and ffprobe on PATH, curl, and a reachable Postgres.
set -euo pipefail

BINARY="${1:-./media/aura-media}"
DSN="${2:-${AURA_MEDIA_TEST_DSN:-}}"
PORT="${MEDIA_PORT:-9390}"
RTMP_PORT="${MEDIA_RTMP_PORT:-19390}"
WORK="$(mktemp -d)"
TOKEN="e2e-$(head -c 18 /dev/urandom | base64 | tr -d '=+/')"
NODE_PID=""
PUSH_PID=""

cleanup() {
  [ -n "$PUSH_PID" ] && kill -9 "$PUSH_PID" 2>/dev/null || true
  [ -n "$NODE_PID" ] && kill "$NODE_PID" 2>/dev/null || true
  sleep 1
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1" >&2; [ -f "$WORK/media.log" ] && tail -30 "$WORK/media.log" >&2; exit 1; }

[ -n "$DSN" ] || fail "no DSN: pass one as \$2 or set AURA_MEDIA_TEST_DSN"
command -v ffmpeg >/dev/null || fail "ffmpeg is not on PATH"

HOST="http://127.0.0.1:$PORT"
api() {
  local method="$1" path="$2"; shift 2
  curl -sS -X "$method" -H "Authorization: Bearer $TOKEN" "$HOST$path" "$@"
}
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }
number() { sed -n "s/.*\"$1\":\([0-9.eE+-]*\).*/\1/p"; }
cache() { curl -s -o /dev/null -D - "$1" | tr -d '\r' | sed -n 's/^[Cc]ache-[Cc]ontrol: //p'; }

# A source with both streams and a length that varies per run, so a re-run
# encodes rather than deduplicating against the last one's digest. Long enough,
# and busy enough, that the encode takes several seconds: the first half of this
# script is about what is true *while* it runs.
#
# The length alone is eleven possibilities, and a database that outlives one
# run — a developer's, not CI's — would deduplicate the twelfth run against the
# first and encode nothing. The title makes the bytes this run's own.
DURATION=$(( 50 + RANDOM % 11 ))
ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "testsrc2=size=1280x720:rate=25:duration=$DURATION" \
  -f lavfi -i "sine=frequency=440:duration=$DURATION" \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -shortest \
  -metadata title="e2e $(date +%s) $RANDOM$RANDOM" "$WORK/source.mp4"
pass "made a ${DURATION}s 720p source with audio"

# One worker, so the encode is not competing with anything and its timing is
# its own. The live window is short so that a 20-second stream slides it.
AURA_MEDIA_TOKEN="$TOKEN" "$BINARY" serve \
  --dsn "$DSN" --bind "127.0.0.1:$PORT" --data "$WORK/data" --workers 1 \
  --rtmp-bind "127.0.0.1:$RTMP_PORT" --live-grace 6s --live-window 8s \
  > "$WORK/media.log" 2>&1 &
NODE_PID=$!
for _ in $(seq 1 60); do
  curl -sf "$HOST/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -sf "$HOST/healthz" >/dev/null || fail "the node never became healthy"
pass "node is up"

[ "$(code "$HOST/readyz")" = "200" ] || fail "readiness is not open, or the node is not ready"
[ "$(code "$HOST/v1/queue")" = "401" ] || fail "the control surface answered without a token"
[ "$(code "$HOST/v1/live")" = "401" ] || fail "the live routes answered without a token"
pass "readiness is open and the control surface is not"

# ---------------------------------------------------------------------------
# An asset in, an address out — and the address before the encode is over.
# ---------------------------------------------------------------------------

# Keyed on the content, which is what an idempotency key is for. A key that
# collides across runs would hand this run the previous one's asset — correct
# idempotency, and a confusing way to fail an end-to-end test.
KEY="e2e-$(sha256sum "$WORK/source.mp4" | cut -c1-32)"
RESPONSE=$(api POST /v1/assets -H "Content-Type: video/mp4" -H "Idempotency-Key: $KEY" --data-binary "@$WORK/source.mp4")
ASSET=$(printf '%s' "$RESPONSE" | field id)
[ -n "$ASSET" ] || fail "the upload returned no asset: $RESPONSE"
pass "upload accepted as $ASSET"

STATE=""
EARLY=""          # the address, the first time it was seen while still processing
EARLY_CHECKED=""
PROGRESS_SEEN=""  # every progress value, in the order it was read
for _ in $(seq 1 600); do
  BODY=$(api GET "/v1/assets/$ASSET")
  STATE=$(printf '%s' "$BODY" | field state)
  PROGRESS_SEEN="$PROGRESS_SEEN $(printf '%s' "$BODY" | number progress)"
  [ "$STATE" = "ready" ] && break
  [ "$STATE" = "failed" ] && fail "the encode failed: $BODY"
  if [ "$STATE" = "processing" ] && [ -z "$EARLY_CHECKED" ]; then
    EARLY=$(printf '%s' "$BODY" | field address)
    if [ -n "$EARLY" ]; then
      EARLY_CHECKED=1
      # The claim under test: this is a real, playable package right now,
      # while the state still says processing.
      printf '%s' "$BODY" | grep -q '"growing":true' || fail "an address was published early without saying the package is still growing: $BODY"
      curl -sf "$HOST$EARLY" | grep -q "720p/index.m3u8" || fail "the early address does not serve a master playlist"
      EARLY_BASE=$(dirname "$EARLY")
      EARLY_LIST=$(curl -sf "$HOST$EARLY_BASE/720p/index.m3u8") || fail "the early 720p playlist does not serve"
      printf '%s' "$EARLY_LIST" | grep -q "#EXT-X-ENDLIST" && fail "the playlist was already closed when the address was first published; nothing was shown to be early"
      FIRST_SEGMENT=$(printf '%s\n' "$EARLY_LIST" | grep -m1 '^seg_')
      [ -n "$FIRST_SEGMENT" ] || fail "the early playlist lists no segment"
      # Every file the early playlist names must already be fetchable: a
      # playlist is only uploaded after what it lists.
      for name in $(printf '%s\n' "$EARLY_LIST" | grep '^seg_'); do
        [ "$(code "$HOST$EARLY_BASE/720p/$name")" = "200" ] || fail "the early playlist lists $name, which does not serve"
      done
      ffmpeg -hide_banner -loglevel error -y -i "$HOST$EARLY" -t 2 -f null - 2>"$WORK/early.log" \
        || fail "ffmpeg could not play the package while it was still being encoded: $(tail -3 "$WORK/early.log")"
      [ "$(cache "$HOST$EARLY_BASE/720p/index.m3u8")" = "public, max-age=1" ] \
        || fail "a playlist that is still growing is cacheable for longer than a second: $(cache "$HOST$EARLY_BASE/720p/index.m3u8")"
      STILL=$(api GET "/v1/assets/$ASSET" | field state)
      [ "$STILL" = "processing" ] || fail "the encode finished while the early address was being checked (state $STILL); the source is too short to prove anything"
    fi
  fi
  sleep 0.3
done
[ "$STATE" = "ready" ] || fail "the asset never became ready (last state: $STATE)"
[ -n "$EARLY_CHECKED" ] || fail "the address was never published while the asset was still processing"
pass "playable at $EARLY while still encoding, with every listed segment served"
pass "encoded and packaged"

# Progress: it must have been somewhere strictly between nothing and done at
# least once, never have gone backwards, and end at 1.
printf '%s\n' $PROGRESS_SEEN | awk '
  { v = $1 + 0; if (v < last) { bad = 1 } last = v; if (v > 0 && v < 1) mid++ }
  END { if (bad) { print "went backwards"; exit 1 } if (mid < 1) { print "was never between 0 and 1" ; exit 1 } if (last != 1) { print "ended at " last; exit 1 } }
' > "$WORK/progress.txt" || fail "progress $(cat "$WORK/progress.txt"): $PROGRESS_SEEN"
pass "progress was reported, only ever went up, and ended at 1"

# None of the above counts if it was the fallback that produced the package.
grep -q "encoding this asset the way 0.1.0 did" "$WORK/media.log" && fail "the new encode failed and the 0.1.0 path rescued it; the log says why"
pass "the single-invocation encode ran; the fallback was not needed"

printf '%s' "$BODY" | grep -q '"growing":true' && fail "a ready asset still says it is growing: $BODY"
ADDRESS=$(printf '%s' "$BODY" | field address)
[ -n "$ADDRESS" ] || fail "a ready asset has no address: $BODY"
[ "$ADDRESS" = "$EARLY" ] || fail "the address changed between early ($EARLY) and ready ($ADDRESS)"
MASTER=$(curl -sf "$HOST$ADDRESS") || fail "the address does not serve: $ADDRESS"
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
[ "$(code "$HOST$BASE/720p/seg_00000.m4s")" = "200" ] || fail "the first segment does not serve"
pass "segments serve"

# Complete means closed: every rung ends with ENDLIST, every rung has the same
# number of segments, and every one of them is in the store.
COUNT=""
for rung in 720p 480p 360p; do
  LIST=$(curl -sf "$HOST$BASE/$rung/index.m3u8") || fail "the $rung playlist does not serve"
  printf '%s\n' "$LIST" | tail -1 | grep -q "#EXT-X-ENDLIST" || fail "the finished $rung playlist does not end with ENDLIST"
  THIS=$(printf '%s\n' "$LIST" | grep -c '^seg_')
  [ -z "$COUNT" ] && COUNT="$THIS"
  [ "$THIS" = "$COUNT" ] || fail "$rung has $THIS segments and another rung has $COUNT: the ladder is not aligned"
  for name in $(printf '%s\n' "$LIST" | grep '^seg_'); do
    [ "$(code "$HOST$BASE/$rung/$name")" = "200" ] || fail "$rung/$name is listed and does not serve"
  done
done
[ "$COUNT" -ge $(( DURATION / 4 )) ] || fail "$COUNT segments for a ${DURATION}s source in 4s segments"
pass "every rung is closed with ENDLIST, has the same $COUNT segments, and all of them serve"

[ "$(cache "$HOST$BASE/720p/index.m3u8")" = "public, max-age=60" ] || fail "a finished playlist lost its minute of caching: $(cache "$HOST$BASE/720p/index.m3u8")"
[ "$(cache "$HOST$BASE/720p/seg_00000.m4s")" = "public, max-age=31536000, immutable" ] || fail "a segment is not served as immutable"
pass "segments are immutable, finished playlists cache for a minute, growing ones for a second"

# The source is the clip before anything de-identified it. It is in the same
# store and it must not be reachable over the public tree, with or without a token.
[ "$(code "$HOST/media/src/$ASSET")" = "404" ] || fail "a source was served over the public tree"
[ "$(code -H "Authorization: Bearer $TOKEN" "$HOST/media/src/$ASSET")" = "404" ] || fail "a source was served to a token holder over the public tree"
pass "sources are not public"

printf '%s' "$BODY" | grep -q '"frames":\[' || fail "no frames were sampled; every AI capability downstream needs them"
printf '%s' "$BODY" | grep -q '"poster":' || fail "no poster was taken"
FRAMES=$(printf '%s' "$BODY" | grep -o 'frames/frame_[0-9]*\.jpg' | sort -u | wc -l)
# One every five seconds; ffmpeg's fps filter rounds at both ends.
[ "$FRAMES" -ge $(( DURATION / 5 - 1 )) ] || fail "$FRAMES frames from a ${DURATION}s source sampled every 5s"
[ "$(code "$HOST$BASE/frames/frame_00001.jpg")" = "200" ] || fail "the first frame does not serve"
pass "$FRAMES frames and a poster, the frames from the same ffmpeg as the ladder"

# The whole point of the package: something can play it.
ffmpeg -hide_banner -loglevel error -y -i "$HOST$ADDRESS" -t 3 -f null - 2>"$WORK/play.log" \
  || fail "ffmpeg could not play the packaged HLS: $(tail -3 "$WORK/play.log")"
pass "ffmpeg plays the packaged HLS back"

AGAIN=$(api POST /v1/assets -H "Content-Type: video/mp4" --data-binary "@$WORK/source.mp4")
printf '%s' "$AGAIN" | grep -q '"deduplicated":true' || fail "the same bytes were accepted for a second encode"
[ "$(printf '%s' "$AGAIN" | field id)" = "$ASSET" ] || fail "the duplicate upload returned a different asset"
pass "identical bytes deduplicate instead of encoding twice"

# ---------------------------------------------------------------------------
# A stream in, an address out.
# ---------------------------------------------------------------------------

CREATED=$(api POST /v1/live)
STREAM=$(printf '%s' "$CREATED" | field id)
STREAM_KEY=$(printf '%s' "$CREATED" | field stream_key)
INGEST=$(printf '%s' "$CREATED" | field ingest_url)
LIVE_ADDRESS=$(printf '%s' "$CREATED" | field address)
LIVE_ASSET=$(printf '%s' "$CREATED" | field asset_id)
[ -n "$STREAM" ] && [ -n "$STREAM_KEY" ] && [ -n "$INGEST" ] && [ -n "$LIVE_ADDRESS" ] \
  || fail "creating a live stream did not return an id, a key, an ingest URL and an address: $CREATED"
api GET "/v1/live/$STREAM" | grep -q "stream_key" && fail "the stream key was returned a second time; it is shown once"
[ "$(api GET "/v1/live/$STREAM" | field state)" = "idle" ] || fail "a new stream is not idle"
[ "$(code "$HOST$LIVE_ADDRESS")" = "404" ] || fail "the live address serves before anyone has broadcast"
pass "created live stream $STREAM; its key was shown once"

# A synthetic broadcaster, in real time. An array rather than a function so
# that a broadcast started in the background is ffmpeg itself and not a shell
# around it: the pid kept is the pid that gets killed.
BROADCAST=(ffmpeg -hide_banner -loglevel error -re
  -f lavfi -i "testsrc2=size=1280x720:rate=30" -f lavfi -i "sine=frequency=330"
  -c:v libx264 -preset ultrafast -tune zerolatency -pix_fmt yuv420p -g 60 -b:v 1500k
  -c:a aac -ar 44100)
push() { "${BROADCAST[@]}" -t "$1" -f flv "$INGEST/$2"; } # push <seconds> <key>

# A key that is one character wrong, for a stream that exists.
WRONG="${STREAM_KEY%?}0"; [ "$WRONG" = "$STREAM_KEY" ] && WRONG="${STREAM_KEY%?}1"
if push 3 "$WRONG" >/dev/null 2>&1; then fail "a broadcast with the wrong key was accepted"; fi
[ "$(api GET "/v1/live/$STREAM" | field state)" = "idle" ] || fail "a refused broadcast changed the stream's state"
pass "a wrong key is refused"

"${BROADCAST[@]}" -t 600 -f flv "$INGEST/$STREAM_KEY" > "$WORK/push1.log" 2>&1 &
PUSH_PID=$!

# Live, and playable, within a few seconds of the broadcaster connecting.
LIVE_UP=""
for _ in $(seq 1 60); do
  if [ "$(code "$HOST$LIVE_ADDRESS")" = "200" ]; then LIVE_UP=1; break; fi
  kill -0 "$PUSH_PID" 2>/dev/null || fail "the broadcaster exited: $(tail -3 "$WORK/push1.log")"
  sleep 0.5
done
[ -n "$LIVE_UP" ] || fail "the live address never started serving"
[ "$(api GET "/v1/live/$STREAM" | field state)" = "live" ] || fail "the stream is being broadcast and is not live"
LIVE_BODY=$(api GET "/v1/assets/$LIVE_ASSET")
[ "$(printf '%s' "$LIVE_BODY" | field state)" = "processing" ] || fail "a live asset is not processing: $LIVE_BODY"
[ "$(printf '%s' "$LIVE_BODY" | field kind)" = "live" ] || fail "a live asset does not say it is live: $LIVE_BODY"
printf '%s' "$LIVE_BODY" | grep -q '"growing":true' || fail "a live asset does not say it is growing: $LIVE_BODY"
pass "live and playable at $LIVE_ADDRESS"

LIVE_BASE=$(dirname "$LIVE_ADDRESS")
last_segment() { curl -sf "$HOST$LIVE_BASE/720p/index.m3u8" | grep '^seg_' | tail -1; }
sequence() { curl -sf "$HOST$LIVE_BASE/720p/index.m3u8" | sed -n 's/^#EXT-X-MEDIA-SEQUENCE://p'; }
FIRST_EDGE=$(last_segment)
sleep 5
SECOND_EDGE=$(last_segment)
[ -n "$FIRST_EDGE" ] && [ "$FIRST_EDGE" != "$SECOND_EDGE" ] || fail "the live playlist did not advance in five seconds (still at $FIRST_EDGE)"
curl -sf "$HOST$LIVE_BASE/720p/index.m3u8" | grep -q "#EXT-X-ENDLIST" && fail "a live playlist is closed"
curl -sf "$HOST$LIVE_BASE/720p/index.m3u8" | grep -q "#EXT-X-TARGETDURATION:2" || fail "live segments are not two seconds"
[ "$(cache "$HOST$LIVE_BASE/720p/index.m3u8")" = "public, max-age=1" ] || fail "a live playlist is cacheable for longer than a second"
[ "$(cache "$HOST$LIVE_BASE/720p/$SECOND_EDGE")" = "public, max-age=31536000, immutable" ] || fail "a live segment is not immutable"
ffmpeg -hide_banner -loglevel error -y -i "$HOST$LIVE_ADDRESS" -t 2 -f null - 2>"$WORK/liveplay.log" \
  || fail "ffmpeg could not play the live stream: $(tail -3 "$WORK/liveplay.log")"
pass "the live playlist advances ($FIRST_EDGE -> $SECOND_EDGE), is not cacheable past a second, and plays"

# The broadcaster's connection dies — killed, not closed, so there is no
# goodbye — and it comes back inside the grace window. Same stream, same asset.
sleep 6
kill -9 "$PUSH_PID" 2>/dev/null || true
wait "$PUSH_PID" 2>/dev/null || true
PUSH_PID=""
BEFORE_DROP=$(last_segment)
sleep 2
[ "$(api GET "/v1/live/$STREAM" | field state)" = "live" ] || fail "the stream ended the moment the broadcaster dropped, instead of waiting out the grace window"
grep -q "broadcaster dropped; holding the stream open" "$WORK/media.log" || fail "the node did not notice the broadcaster drop"
push 9 "$STREAM_KEY" > "$WORK/push2.log" 2>&1 || fail "the broadcaster could not reconnect: $(tail -3 "$WORK/push2.log")"
pass "the broadcaster dropped and reconnected inside the grace window"

# The second push ran its nine seconds and said goodbye, which ends the stream.
LIVE_STATE=""
for _ in $(seq 1 120); do
  LIVE_STATE=$(api GET "/v1/live/$STREAM" | field state)
  [ "$LIVE_STATE" = "ended" ] && break
  sleep 0.5
done
[ "$LIVE_STATE" = "ended" ] || fail "the stream did not end after the broadcaster stopped (state: $LIVE_STATE)"
VOD=$(api GET "/v1/assets/$LIVE_ASSET")
[ "$(printf '%s' "$VOD" | field state)" = "ready" ] || fail "the ended stream is not a ready asset: $VOD"
printf '%s' "$VOD" | grep -q '"growing":true' && fail "the ended stream's asset still says it is growing"
[ "$(printf '%s' "$VOD" | field address)" = "$LIVE_ADDRESS" ] || fail "the VOD is not at the address the stream was at"
VOD_LIST=$(curl -sf "$HOST$LIVE_BASE/720p/index.m3u8")
printf '%s\n' "$VOD_LIST" | tail -1 | grep -q "#EXT-X-ENDLIST" || fail "the VOD playlist is not closed with ENDLIST"
printf '%s' "$VOD_LIST" | grep -q "#EXT-X-PLAYLIST-TYPE:VOD" || fail "the VOD playlist does not say it is a VOD"
[ "$(printf '%s\n' "$VOD_LIST" | sed -n 's/^#EXT-X-MEDIA-SEQUENCE://p')" = "0" ] || fail "the VOD playlist does not start at the beginning of the stream"
printf '%s\n' "$VOD_LIST" | grep -q '^seg_00000.m4s' || fail "the VOD lost the start of the stream; the live window was all that was kept"
VOD_SEGMENTS=$(printf '%s\n' "$VOD_LIST" | grep -c '^seg_')
# About eleven seconds before the drop and nine after: twenty seconds, ten
# segments. Both sides of the reconnect must be in there.
[ "$VOD_SEGMENTS" -ge 8 ] || fail "the VOD has $VOD_SEGMENTS two-second segments from about twenty seconds of broadcast"
LAST_VOD=$(printf '%s\n' "$VOD_LIST" | grep '^seg_' | tail -1)
[ "$LAST_VOD" \> "$BEFORE_DROP" ] || fail "nothing after the reconnect made it into the VOD (last segment $LAST_VOD, last before the drop $BEFORE_DROP)"
for name in $(printf '%s\n' "$VOD_LIST" | grep '^seg_'); do
  [ "$(code "$HOST$LIVE_BASE/720p/$name")" = "200" ] || fail "the VOD lists $name, which does not serve"
done
[ "$(code "$HOST$LIVE_BASE/720p/dvr.m3u8")" = "404" ] || fail "the whole-stream playlist was left behind after the stream ended"
# Played to the end, across the reconnect: this is what fails if the two
# connections were not stitched into one timeline.
ffmpeg -hide_banner -loglevel error -y -i "$HOST$LIVE_ADDRESS" -f null - 2>"$WORK/vodplay.log" \
  || fail "ffmpeg could not play the VOD of the stream: $(tail -3 "$WORK/vodplay.log")"
VOD_SECONDS=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$HOST$LIVE_ADDRESS" | cut -d. -f1)
[ "${VOD_SECONDS:-0}" -ge 15 ] || fail "the VOD is ${VOD_SECONDS}s long from about twenty seconds of broadcast"
pass "the stream closed into a ${VOD_SECONDS}s VOD of $VOD_SEGMENTS segments at the same address, from its first segment, across the reconnect"

if push 3 "$STREAM_KEY" >/dev/null 2>&1; then fail "the key of an ended stream was accepted again"; fi
pass "the key of an ended stream is dead"

# The key is a credential. Its secret half must not be in the log, however the
# broadcasts above were logged.
SECRET="${STREAM_KEY#*_}"
grep -q "$SECRET" "$WORK/media.log" && fail "the stream key was written to the log"
pass "the stream key was never logged"

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
