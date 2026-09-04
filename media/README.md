# aura-media

An asset in, an address out. Upload a video; get back an HLS package a player
can fetch, still frames sampled from the same decode, and a poster.

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

See [ROADMAP](../ROADMAP.md#media-and-the-ai-over-it) for why this exists and
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

## How it is put together

| | |
|---|---|
| `internal/queue` | Postgres, `FOR UPDATE SKIP LOCKED`. A claim is a lease; a worker that dies loses the job rather than taking it with it |
| `internal/pipeline` | Every ffmpeg command as a value a test can read, and a thin runner over it |
| `internal/store` | Bytes. Sources under `src/`, packages under `out/`, and only `out/` is public |
| `internal/worker` | N of these. The worker count *is* the parallelism |
| `internal/ingest` | The one way bytes enter, so two doors cannot grow two sets of rules |
| `internal/api` | The control surface (token) and the output tree (open) |
| `internal/c3` | The connection to a kernel |

## Checks

```bash
gofmt -l . && go vet ./... && go test ./... -cover
AURA_MEDIA_TEST_DSN=postgres://... go test ./...   # queue and API against a real server
../scripts/media_e2e.sh ./aura-media               # needs ffmpeg; encodes for real
```

Tests that need Postgres skip without `AURA_MEDIA_TEST_DSN`, so `go test ./...`
still runs on a machine with no database. The end-to-end script is where the
encode actually happens, and it is where the bugs were: an idempotency key
honoured after the upload rather than before it, and a rendition width two
pixels off what the playlist claimed.

## Not built yet

The pipeline is the floor the roadmap's four capabilities stand on; none of them
is here. De-identifying a clip, transcribing and subtitling, drafting a
moderation judgement, suggesting a poster and alt text: all four need frames,
which is why frames come out of the first encode rather than a later one.
LiveKit ingress and egress belong in this subsystem too and are not started.

Also outstanding: an S3-compatible store beside the filesystem one (the
interface is there, the implementation is not), Shaka Packager where ffmpeg's
HLS muxer is not enough, and per-job progress rather than a state that only
moves at the end.
