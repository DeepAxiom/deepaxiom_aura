// Package ingest is the one way bytes enter this subsystem.
//
// There is exactly one of these on purpose: the HTTP API and the C3 skill are
// two doors into the same room, and a second ingest path is how the two grow
// different size limits, different deduplication and different idempotency
// without anyone deciding that they should.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"aura/media/internal/assets"
	"aura/media/internal/id"
	"aura/media/internal/queue"
	"aura/media/internal/store"
)

// DefaultMaxBytes bounds one source. Large enough for an hour of clinic
// recording, small enough that a single request cannot fill the disk.
const DefaultMaxBytes int64 = 8 << 30 // 8 GiB

// Catalogue is the part of the asset repository ingest needs. It is an
// interface so this package's rules — the size limit, deduplication, what
// happens when the queue refuses — can be tested on a machine with no database.
type Catalogue interface {
	Create(ctx context.Context, a assets.Asset) (assets.Asset, error)
	Get(ctx context.Context, id string) (assets.Asset, error)
	BySourceDigest(ctx context.Context, sha256 string) (assets.Asset, error)
	Delete(ctx context.Context, id string) error
}

// Jobs is the part of the queue ingest needs.
type Jobs interface {
	Enqueue(ctx context.Context, n queue.New) (queue.Job, error)
	ByIdem(ctx context.Context, idem string) (queue.Job, error)
	ForAsset(ctx context.Context, assetID string) ([]queue.Job, error)
}

// Ingestor stores a source, records the asset and enqueues the work.
type Ingestor struct {
	Assets   Catalogue
	Queue    Jobs
	Store    store.Store
	MaxBytes int64
	// HTTP fetches sources named by URI. Nil means URI ingest is refused,
	// which is the right default for a node that should only take uploads.
	HTTP *http.Client
}

// Request is what a caller knows about the source before it is read.
type Request struct {
	MIME string
	// Idem makes a redelivered request find its own job instead of starting a
	// second encode. The HTTP API passes Idempotency-Key; the C3 skill passes
	// the envelope's `idem`, which C3 requires receivers to deduplicate by.
	Idem     string
	Priority int
}

// Result is what a caller gets back: the asset, and the job that will finish it.
type Result struct {
	Asset assets.Asset
	Job   queue.Job
	// Deduplicated is true when these exact bytes were already here. The
	// caller is handed the existing asset rather than a second encode of the
	// same file.
	Deduplicated bool
}

// ErrTooLarge is returned when a source exceeds the limit. It is deliberately
// not a truncation: half a video stored as if it were whole is worse than a
// refusal, because nothing downstream can tell.
var ErrTooLarge = errors.New("source is larger than this node accepts")

// FromReader stores a source and queues its transcode.
func (in *Ingestor) FromReader(ctx context.Context, r io.Reader, req Request) (Result, error) {
	if in.Assets == nil || in.Queue == nil || in.Store == nil {
		return Result{}, errors.New("ingestor is not wired up")
	}
	// The idempotency key first, before a byte is read. It names a request that
	// has already been answered, and the expensive way to discover that is to
	// pull a video off the wire and hash it. This is also the only thing that
	// makes the key safe: a key answered for one file must never be answered
	// with a different file's work, and checking after the fact leaves an asset
	// stored with somebody else's job attached to it — which is a row nothing
	// will ever encode and a caller waiting for an address that never comes.
	if req.Idem != "" {
		if existing, err := in.answeredBefore(ctx, req.Idem); err == nil {
			return existing, nil
		} else if !errors.Is(err, queue.ErrNoJob) {
			return Result{}, err
		}
	}

	assetID := id.New()
	key := path.Join("src", assetID)

	limit := in.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	obj, err := in.Store.Put(ctx, key, &limitedReader{r: r, remaining: limit + 1, limit: limit})
	if err != nil {
		return Result{}, err
	}
	if obj.Bytes == 0 {
		return Result{}, errors.New("the source is empty")
	}

	// Same bytes, already here: hand back what exists. An encode is minutes of
	// a pinned core, so doing it twice for one file is the most expensive
	// possible way to get the same address.
	if existing, err := in.Assets.BySourceDigest(ctx, obj.SHA256); err == nil {
		jobs, jerr := in.Queue.ForAsset(ctx, existing.ID)
		if jerr == nil && len(jobs) > 0 {
			return Result{Asset: existing, Job: jobs[0], Deduplicated: true}, nil
		}
	} else if !errors.Is(err, assets.ErrNotFound) {
		return Result{}, err
	}

	asset, err := in.Assets.Create(ctx, assets.Asset{
		ID:           assetID,
		SourceKey:    obj.Key,
		SourceBytes:  obj.Bytes,
		SourceSHA256: obj.SHA256,
		MIME:         strings.TrimSpace(req.MIME),
	})
	if err != nil {
		return Result{}, err
	}
	job, err := in.Queue.Enqueue(ctx, queue.New{
		AssetID:  asset.ID,
		Kind:     queue.KindTranscode,
		Priority: req.Priority,
		Idem:     req.Idem,
	})
	if err != nil {
		// The bytes are stored and the row exists, but nothing will ever run.
		// Saying so is the only honest answer: a 202 here would promise an
		// address that never arrives.
		return Result{}, fmt.Errorf("asset %s was stored but not queued: %w", asset.ID, err)
	}
	// The key was claimed between the check above and here — two requests
	// carrying it arrived at once. The other one won; this asset is an orphan
	// and is removed rather than left in "received" forever.
	if job.AssetID != asset.ID {
		in.discard(ctx, asset)
		if answered, err := in.answeredBefore(ctx, req.Idem); err == nil {
			return answered, nil
		}
		return Result{}, fmt.Errorf("idempotency key %q belongs to a different asset", req.Idem)
	}
	return Result{Asset: asset, Job: job}, nil
}

// answeredBefore returns what an idempotency key already produced.
func (in *Ingestor) answeredBefore(ctx context.Context, idem string) (Result, error) {
	job, err := in.Queue.ByIdem(ctx, idem)
	if err != nil {
		return Result{}, err
	}
	asset, err := in.Assets.Get(ctx, job.AssetID)
	if err != nil {
		return Result{}, fmt.Errorf("idempotency key %q names job %d, whose asset cannot be read: %w", idem, job.ID, err)
	}
	return Result{Asset: asset, Job: job, Deduplicated: true}, nil
}

// discard removes an asset this node should not have created, bytes and all.
// Failing to clean up is logged by the caller's error rather than hidden, but it
// does not fail the request: the caller's work exists either way.
func (in *Ingestor) discard(ctx context.Context, asset assets.Asset) {
	_ = in.Store.Delete(ctx, asset.SourceKey)
	_ = in.Assets.Delete(ctx, asset.ID)
}

// FromURI fetches a source this node can reach and ingests it.
//
// file:// is for an operator pointing at something already on the box; http(s)
// is what a C3 envelope carries, since an envelope names a video rather than
// carrying one. Anything else is refused by name, so a scheme nobody thought
// about is a clear error rather than a surprise.
func (in *Ingestor) FromURI(ctx context.Context, uri string, req Request) (Result, error) {
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return Result{}, fmt.Errorf("unreadable source URI: %w", err)
	}
	switch parsed.Scheme {
	case "file":
		local := parsed.Path
		if len(local) > 2 && local[0] == '/' && local[2] == ':' {
			local = local[1:] // /C:/x on Windows
		}
		f, err := os.Open(local)
		if err != nil {
			return Result{}, fmt.Errorf("cannot read %s: %w", uri, err)
		}
		defer f.Close()
		return in.FromReader(ctx, f, req)
	case "http", "https":
		if in.HTTP == nil {
			return Result{}, errors.New("this node does not fetch sources over HTTP; upload the bytes instead")
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return Result{}, err
		}
		resp, err := in.HTTP.Do(httpReq)
		if err != nil {
			return Result{}, fmt.Errorf("fetching %s: %w", uri, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return Result{}, fmt.Errorf("fetching %s: the server answered %s", uri, resp.Status)
		}
		if req.MIME == "" {
			req.MIME = resp.Header.Get("Content-Type")
		}
		return in.FromReader(ctx, resp.Body, req)
	default:
		return Result{}, fmt.Errorf("source URI scheme %q is not one this node fetches (file, http, https)", parsed.Scheme)
	}
}

// limitedReader fails at the limit rather than stopping there. io.LimitReader
// would return a clean EOF, which stores a truncated video as if it were whole.
type limitedReader struct {
	r         io.Reader
	remaining int64
	limit     int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, fmt.Errorf("%w (%d bytes)", ErrTooLarge, l.limit)
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining <= 0 && err == nil {
		return n, fmt.Errorf("%w (%d bytes)", ErrTooLarge, l.limit)
	}
	return n, err
}
