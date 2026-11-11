# aura-media

An asset in, an address out. Upload a video; get back an HLS package a player
can fetch, still frames sampled from the same decode, and a poster. Since 0.2.0
the address works a few seconds after the encode starts rather than when it
ends, and the same thing works for a stream: push RTMP, get an address that
plays live and is a VOD when the broadcast stops.

This is a **second artefact**, versioned apart from the kernel ([VERSION](VERSION),
its own `go.mod`). It runs *beside* a node, not inside one. Three reasons, none
of them style:

- **Opposite load curves.** Approving an effect is milliseconds and must never
  queue; encoding is a core at 100% for minutes. One process means a video
  upload can starve a clinician waiting to sign a note.
- **Zone crossing.** The kernel is where PHI dictation is handled. The bytes
  here are public-zone by construction, and the sources it holds are served to
  nobody.
- **A governable pin.** A consumer pins one commit to say which kernel sealed a
  note. If libav shipped in that artefact, every codec CVE would force a kernel
  version bump. Here it forces a bump of this line instead.

See [ROADMAP](../ROADMAP.md#video) for why this exists and
what is meant to be built on it.

## Run it

Needs `ffmpeg` and `ffprobe` on PATH, and a Postgres.

```bash
go build -ldflags "-X main.version=$(cat VERSION)" -o aura-media ./cmd/aura-media

./aura-media serve \
  --dsn "postgres://user:pass@localhost:5432/media" \
  --data ~/.aura-media \
  --workers 4
```

It mints a token into `<data>/media.token` on first start and prints where it
is listening. `--no-auth` exists and is refused on any bind that is not
loopback. `aura-media migrate --print` shows the DDL before it runs.

```bash
TOKEN=$(cat ~/.aura-media/media.token)

# Upload. 202 means queued; 200 means nothing new was queued (same bytes, or an
# idempotency key already answered).
curl -X POST http://127.0.0.1:9090/v1/assets \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: video/mp4" \
  -H "Idempotency-Key: $(sha256sum clip.mp4 | cut -c1-32)" \
  --data-binary @clip.mp4

# Poll until state is ready, then play `address`.
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9090/v1/assets/<id>
```

`address` needs no token: it is what a player fetches. Everything else does.

### Playable before it is finished

`state: ready` means what it always meant: the package is complete. What changed
is that `address` now appears while `state` is still `processing`, as soon as the
first segment of every rung is in the store, with two fields beside it:

| | |
|---|---|
| `growing` | `true` while the address plays but segments are still being added. Absent once ready. |
| `progress` | 0 to 1, from ffmpeg's own report against the probed duration. Written every couple of seconds, never backwards within an attempt, and 1 only when the asset is ready. |
| `kind` | `vod` or `live`. |

A client that only waits for `ready` is unaffected. A client that treated a
non-empty `address` as "finished" is not: it will now see one early. That is the
one thing in this release an existing consumer can notice, and
`--legacy-encode` turns it off.

While it grows, the package is an HLS EVENT playlist: entries are only ever
appended, and ENDLIST closes it. A player given a playlist with no ENDLIST
treats it as live and starts near the end of what exists so far; it does not
start from zero until the encode has finished. Nothing here rewrites that.

A segment is uploaded once the rung's playlist lists it, because ffmpeg lists a
segment only after closing the file; the file it is still writing is never read.
A playlist is uploaded only after every file it names. If the encode fails, the
address is withdrawn, and the next attempt deletes what the failed one published
before it writes anything. Segments are served `immutable`, so a CDN that cached
a segment of a failed attempt may serve it in place of the retry's; both are
encodes of the same frames cut at the same instants.

### Fallback

The new encode is one ffmpeg invocation: the ladder and the frames from one
decode, `-progress` on stdout, segments published as they close. If it fails for
any reason — ffmpeg refuses the argv, the rungs come out misaligned, the master
playlist has no CODECS — the worker logs why and encodes the asset again exactly
as 0.1.0 did: libx264 at fixed bitrates, frames from a second pass, the tree
uploaded at the end. The job does not fail and no attempt is spent. A source
that neither path can encode costs two ffmpeg runs per attempt instead of one.

`--legacy-encode` (or `AURA_MEDIA_LEGACY_ENCODE=1`) skips the new path entirely.

### Encoder

| Flag | Env | Default |
|---|---|---|
| `--video-encoder` | `AURA_MEDIA_VIDEO_ENCODER` | `libx264` |
| `--preset` | `AURA_MEDIA_PRESET` | per encoder (`veryfast` for libx264 and libx265, `10` for SVT-AV1, none for hardware) |
| `--rate-mode` | `AURA_MEDIA_RATE_MODE` | `cbr-ladder` |
| `--crf` | `AURA_MEDIA_CRF` | 23 / 26 / 35 for x264 / x265 / SVT-AV1 |

`cbr-ladder` is 0.1.0's fixed bitrate per rung. `capped-crf` is per-title: one
quality for every rung, with the rung's bitrate as a ceiling, so a slide deck
comes out at a fraction of the bitrate and an action scene hits the cap. With no
flags set the argv for the ladder is byte for byte 0.1.0's, and a test holds it
there against a recording of the old builder's output.

| Encoder | Modes | Verified by running it |
|---|---|---|
| `libx264` | both | Yes, ffmpeg 6.1 and 8.1 |
| `libx265` | both | Yes, ffmpeg 6.1 and 8.1. HEVC in fMP4, tagged `hvc1` |
| `libsvtav1` | `capped-crf` only | Yes on ffmpeg 8.1. **Refused on 6.1**: that muxer writes AV1 variants with no CODECS |
| `h264_nvenc`, `h264_qsv` | `cbr-ladder` only | **No.** ffmpeg 6.1 parses the argv and fails opening the device; no such hardware was available |
| `h264_videotoolbox` | `cbr-ladder` only | **No.** Not even parsed: no macOS ffmpeg was available |

Alignment across rungs is not taken on trust for any of them. With a
non-default encoder the node encodes nine seconds of test pattern at start-up
and refuses to start unless the rungs are cut at the same instants and the
master playlist names a codec for each. The same two checks run on the output of
every job, and a ladder that fails them is re-encoded by the fallback rather
than published. For a live stream there is no second chance: misalignment is
logged as an error and the stream is published as it is.

### Live

```bash
./aura-media serve --dsn ... --rtmp-bind 0.0.0.0:1935 --rtmp-url rtmp://live.example.com/live

curl -X POST -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9090/v1/live
# {"id": "...", "state": "idle", "asset_id": "...",
#  "ingest_url": "rtmp://live.example.com/live",
#  "stream_key": "<id>_<48 hex>",          <- shown once, here, and never again
#  "address": "/media/out/<asset_id>/master.m3u8"}
```

Point an encoder at `ingest_url` with `stream_key` as the stream key (OBS:
Server and Stream Key; ffmpeg: `-f flv <ingest_url>/<stream_key>`). `address`
starts serving about three seconds after the broadcaster connects and is the
same address the VOD is at afterwards. `GET /v1/live`, `GET /v1/live/{id}` and
`POST /v1/live/{id}/end` do what they say; ending an idle stream revokes its
key, ending a live one cuts the broadcast off and keeps what there was.

| Flag | Default | |
|---|---|---|
| `--rtmp-bind` | empty: live is off, and the routes answer 503 saying so | where broadcasters connect |
| `--rtmp-url` | derived from the bind; `127.0.0.1` for a wildcard bind | what broadcasters are told to push to |
| `--live-grace` | `30s` | how long a dropped broadcaster has to come back |
| `--live-window` | `2m` | how much of the stream the live playlist lists; `0` for all of it |

What it is:

- **Two-second segments, the same ladder, one ffmpeg per stream.** Latency is
  the segments: a player holds about three, so expect six to ten seconds behind
  the broadcaster. That figure is arithmetic, not a measurement; glass-to-glass
  delay has not been measured. **There is no LL-HLS.** ffmpeg's HLS muxer does not write
  partial segments (`EXT-X-PART`); getting under about five seconds means a
  different packager, and that is not built.
- **A sliding window while live, the whole stream afterwards.** `index.m3u8` in
  each rung lists the last `--live-window`. Every segment is kept regardless,
  `dvr.m3u8` (per rung, and one at the root as a master) lists the stream from
  its first segment for a player that wants to rewind, and when the stream ends
  `index.m3u8` becomes the whole stream, `PLAYLIST-TYPE:VOD`, closed with
  ENDLIST, and the asset is `ready`. At that moment `EXT-X-MEDIA-SEQUENCE` in
  `index.m3u8` goes from the window's position back to 0. Each segment keeps
  the number it had, but the HLS specification says the sequence must not
  decrease, and **no player has been watched across that moment**: the VOD is
  tested by playing it from the start, not by a viewer who was already
  watching live. `--live-window 0` avoids the question by never windowing, at
  the price of a playlist that grows with the stream.
- **Reconnects continue the stream.** A broadcaster whose connection drops has
  `--live-grace` to come back with the same key; it is the same stream and the
  same asset. There is no `EXT-X-DISCONTINUITY`, because there is no
  discontinuity: the stream is re-encoded, the reconnect's timestamps are
  shifted to follow on from where the last connection stopped, and ffmpeg sees
  one timeline. The time the broadcaster was gone is not in the recording. A
  broadcaster that says goodbye (stops streaming in the encoder) ends the stream
  at once; only a dropped connection gets the grace. The ladder is decided from
  the first connection; a reconnect at a different resolution or codec has not
  been tested.
- **A stream key is used once.** One key, one broadcast, one asset. A product
  that wants a permanent channel key maps it to a new stream per broadcast.
- **Keys.** 192 random bits, shown once. The database holds the SHA-256, the
  comparison is constant-time, every refusal looks the same to the client, and
  the key is never logged and never on ffmpeg's command line — which is why
  ingest is a few hundred lines of RTMP here rather than `ffmpeg -listen`, which
  does not check the stream name at all. **RTMP itself is plaintext**: the key
  crosses the network in the clear unless something in front terminates RTMPS.
  There is no rate limit on wrong keys.
- **Viewers do not connect to this process.** A viewer fetches playlists and
  segments, which are files. Segments are served `immutable`; a playlist that is
  still growing is served `max-age=1`, a finished one `max-age=60`. Put a CDN in
  front and ten thousand viewers are one request a second per playlist to this
  node. That is the whole scaling story for viewers, and nothing else was built
  for it.
- **A node that stops closes its streams.** SIGTERM ends each stream, lets
  ffmpeg finish its segment, and closes the VOD before the process exits. A node
  that is killed cannot; the next node to start, or any other node on the same
  database and store, notices the lapsed lease within about 45 seconds and
  closes what was published from the store alone. With the filesystem store
  "any other node" means the same machine.

### As a skill

```bash
./aura-media serve --dsn ... --node ws://127.0.0.1:9080/ws/skill --fetch-http
```

It registers as `logical.media.transcode` (C1 `format: projection` — the kernel
talks to a service it does not run). An envelope naming a video arrives on
`asset_in`, an envelope naming the packaged one leaves on `asset_out`, both
carrying [`std/media-asset@1`](../spec/schemas/std/media-asset.schema.json).
Registration is optional: a kernel that is down must not stop this serving the
consumers that call it directly.

That contract is unchanged by 0.2.0, and live is not registered as a capability.
A transcode is one envelope in and one out; a live stream is neither, and
describing it would take a schema that does not exist yet.

## Upgrading from 0.1.0

Start the new binary against the old database. The schema is applied on every
start and everything 0.2.0 adds is an addition: three columns on `media_asset`
with defaults, and one new table. Old rows read back as they were; an asset
0.1.0 finished reads as `progress: 1`. A 0.1.0 node still pointed at the same
database keeps working, so a rolling deploy is safe, and it never sees a live
stream because those are not queue jobs.

Adding the columns needs an exclusive lock on `media_asset`, once, for an
instant. If something else is holding the table at that moment — a `pg_dump`
holds it for the length of the backup — the first start waits ten seconds, then
exits saying so, with nothing changed. Start it again when the backup is done.
Every later start reads the catalogue, finds the columns, and takes no lock on
the table at all.

The schema is now applied under an advisory lock. Without it, several nodes
starting at once could race each other's `CREATE TABLE IF NOT EXISTS` and one
would fail to start; that was true of 0.1.0 too.

## How it is put together

| | |
|---|---|
| `internal/queue` | Postgres, `FOR UPDATE SKIP LOCKED`. A claim is a lease; a worker that dies loses the job rather than taking it with it |
| `internal/pipeline` | Every ffmpeg command as a value a test can read, a thin runner over it, and the parsers for what ffmpeg writes back: progress and playlists |
| `internal/publish` | Moves a package into the store while ffmpeg is still writing it. Used by a VOD job and by a live stream, unchanged between them |
| `internal/store` | Bytes. Sources under `src/`, packages under `out/`, and only `out/` is public |
| `internal/worker` | N of these. The worker count *is* the parallelism |
| `internal/rtmp` | Enough RTMP to accept a publisher and hand its audio and video on. No playback, no codecs |
| `internal/live` | A stream's life: key, probe, encode, publish, reconnect, close into a VOD |
| `internal/ingest` | The one way bytes enter, so two doors cannot grow two sets of rules |
| `internal/api` | The control surface (token) and the output tree (open) |
| `internal/c3` | The connection to a kernel |
| `internal/fakeffmpeg` | Tests only: the test binary re-executes itself as ffmpeg, so what happens around an encode can be tested with no codec and with failures on cue |

## Checks

```bash
gofmt -l . && go vet ./... && go test ./... -cover
AURA_MEDIA_TEST_DSN=postgres://... go test ./...   # queue, API, worker, live against a real server
../scripts/media_e2e.sh ./aura-media               # needs ffmpeg; encodes and broadcasts for real
```

Tests that need Postgres skip without `AURA_MEDIA_TEST_DSN`, and tests that need
ffmpeg skip without it on PATH, so `go test ./...` still runs on a machine with
neither. The end-to-end script is where the encode and the broadcast actually
happen, and it is where the bugs were: an idempotency key honoured after the
upload rather than before it, a rendition width two pixels off what the playlist
claimed, and an RTMP goodbye answered by hanging up before the encoder had
finished saying it.

## Releasing

Its own tag line, `media/v<version>`, never the kernel's `v*`:

```bash
# 1. bump and commit
echo 0.3.0 > VERSION && git commit -am "media: 0.3.0"
# 2. tag exactly what VERSION says
git tag media/v0.3.0 && git push origin media/v0.3.0
```

`.github/workflows/release-media.yml` refuses a tag that disagrees with
[VERSION](VERSION) before it builds anything — a binary reporting one number
under a tag naming another is a lie told to whoever pinned it. It then builds
six platforms, generates an SBOM for *this* module, and signs the checksums and
the SBOM keylessly with cosign. Verification is in
[SECURITY.md](../SECURITY.md#verifying-a-release), and the identity to check is
this workflow's, not the kernel's.

## Not built yet

The pipeline is the floor the roadmap's four capabilities stand on; none of them
is here. De-identifying a clip, transcribing and subtitling, drafting a
moderation judgement, suggesting a poster and alt text: all four need frames,
which is why frames come out of the first encode rather than a later one.
LiveKit ingress and egress belong in this subsystem too and are not started.

What "a Twitch could be built on this" still lacks, stated plainly:

- **Low latency.** No LL-HLS and no WebRTC. Two-second segments are what there is.
- **SRT ingest.** Not built. ffmpeg's SRT listener wants a port per stream and
  the passphrase on its command line, and neither fits a door that checks a key
  before anything is encoded.
- **RTMPS.** Terminate TLS in front.
- **A store that outlives the machine.** Still only the filesystem. The
  `Store` interface is unchanged and an S3-compatible implementation is still
  not written; until it is, "another node closes a dead node's stream" is a
  sentence about one machine.
- **Persistent channel keys, chat, viewer counts, a player.** None.
- **Hardware encoding that has been seen to work.** The argv is built and
  guarded; nobody has run it on a GPU.
- **Passthrough.** Every rung is re-encoded, including the top one. A
  broadcaster already sending good 1080p pays for a 1080p encode anyway.
- **Frames from a live stream** stop at `--max-frames`: sampled every
  `--frame-every` seconds from the start, so the default covers the first
  sixteen minutes.

Also outstanding from before: Shaka Packager where ffmpeg's HLS muxer is not
enough, and chunk-parallel encoding of long sources.
