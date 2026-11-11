// Package worker is the part that pins a core.
//
// N of these claim jobs and run the pipeline. They are the reason this whole
// subsystem is a second process: approving an effect in the kernel is
// milliseconds and must never queue, and this is a core at 100% for minutes.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aura/media/internal/assets"
	"aura/media/internal/id"
	"aura/media/internal/pipeline"
	"aura/media/internal/publish"
	"aura/media/internal/queue"
	"aura/media/internal/store"
)

// Config is how a pool is wired.
type Config struct {
	Queue  *queue.Queue
	Assets *assets.Repo
	Store  store.Store
	Runner pipeline.Runner
	Log    *slog.Logger

	// Count is how many jobs run at once. This is the parallelism: there is no
	// other knob, which is the point of a queue with SKIP LOCKED.
	Count int
	// WorkDir is where a job decodes and encodes. Local disk, not the store:
	// ffmpeg writes thousands of small segment files and reads them back.
	WorkDir string
	// Lease is how long a claim is held between heartbeats. A worker that dies
	// loses its job after this.
	Lease time.Duration
	// Idle is how long a worker waits after finding nothing.
	Idle time.Duration
	// FrameEvery and MaxFrames bound the still-frame sampling every AI
	// capability downstream needs.
	FrameEvery float64
	MaxFrames  int

	// Encoding is the operator's choice of encoder and rate mode. The zero
	// value is what 0.1.0 hard-wired.
	Encoding pipeline.Encoding
	// Legacy turns off everything 0.2.0 added to the encode: no publishing as
	// it goes, no progress, frames from a second decode, libx264 at fixed
	// bitrates. It is the switch an operator reaches for when the new path is
	// suspected of something, and it is the same code the new path falls back
	// to on its own when it fails.
	Legacy bool
	// SyncEvery is how often finished segments are looked for and uploaded
	// while ffmpeg runs.
	SyncEvery time.Duration
	// ProgressEvery is the least time between two writes of a job's progress.
	// ffmpeg reports every second; the database does not need to hear it that
	// often, and an hour-long encode should not be 3600 updates of one row.
	ProgressEvery time.Duration
}

func (c *Config) withDefaults() {
	if c.Count <= 0 {
		c.Count = 2
	}
	if c.Lease <= 0 {
		c.Lease = 2 * time.Minute
	}
	if c.Idle <= 0 {
		c.Idle = 2 * time.Second
	}
	if c.FrameEvery <= 0 {
		c.FrameEvery = 5
	}
	if c.MaxFrames <= 0 {
		c.MaxFrames = 200
	}
	if c.SyncEvery <= 0 {
		c.SyncEvery = 500 * time.Millisecond
	}
	if c.ProgressEvery <= 0 {
		c.ProgressEvery = 2 * time.Second
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Pool runs the workers and the reclaimer.
type Pool struct {
	cfg Config
}

// New prepares a pool.
func New(cfg Config) (*Pool, error) {
	if cfg.Queue == nil || cfg.Assets == nil || cfg.Store == nil {
		return nil, errors.New("a worker pool needs a queue, an asset repo and a store")
	}
	cfg.withDefaults()
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(os.TempDir(), "aura-media")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("work directory: %w", err)
	}
	return &Pool{cfg: cfg}, nil
}

// Run blocks until ctx is cancelled, then waits for the workers to put down
// whatever they were holding.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Count; i++ {
		name := fmt.Sprintf("w-%s", id.New()[10:16])
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.loop(ctx, name)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.reclaim(ctx)
	}()
	wg.Wait()
}

func (p *Pool) loop(ctx context.Context, name string) {
	log := p.cfg.Log.With("worker", name)
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := p.cfg.Queue.Claim(ctx, name, p.cfg.Lease)
		if errors.Is(err, queue.ErrNoJob) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.cfg.Idle):
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A queue that cannot be reached is an operational problem, not a
			// job failure. Say so and back off, rather than spinning on it.
			log.Error("cannot claim work", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		p.run(ctx, log, name, job)
	}
}

// run executes one job and records what happened to it. It always leaves the
// row in a state somebody else can act on: done, failed, or back in the queue.
func (p *Pool) run(ctx context.Context, log *slog.Logger, worker string, job queue.Job) {
	log = log.With("job", job.ID, "asset", job.AssetID, "kind", job.Kind, "attempt", job.Attempts)
	started := time.Now()
	log.Info("job started")

	// The heartbeat renews the lease while the job runs, and cancels the job if
	// it ever loses it: a worker that kept encoding after being reclaimed would
	// be a second worker writing the same output.
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	beat := p.heartbeat(jobCtx, cancelJob, log, worker, job.ID)

	err := p.execute(jobCtx, log, job)
	beat()

	// Shutdown: hand the job straight back, unspent. A deploy is not an attempt.
	if ctx.Err() != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if rerr := p.cfg.Queue.Release(releaseCtx, job.ID, worker, "the node stopped while this job was running"); rerr != nil {
			log.Error("could not hand the job back", "error", rerr)
		} else {
			log.Info("job handed back for another worker", "held", time.Since(started).Round(time.Millisecond))
		}
		return
	}

	if err == nil {
		if cerr := p.cfg.Queue.Complete(ctx, job.ID, worker); cerr != nil {
			log.Error("finished the work but could not close the job", "error", cerr)
			return
		}
		log.Info("job done", "took", time.Since(started).Round(time.Millisecond))
		return
	}

	retrying, ferr := p.cfg.Queue.Fail(ctx, job.ID, worker, err)
	if ferr != nil {
		log.Error("could not record the failure", "error", ferr, "cause", err)
		return
	}
	detail := err.Error()
	if retrying {
		log.Warn("job failed, will retry", "error", err)
		if uerr := p.cfg.Assets.Requeued(ctx, job.AssetID, detail); uerr != nil {
			log.Error("could not update the asset", "error", uerr)
		}
		return
	}
	log.Error("job failed for good", "error", err)
	if uerr := p.cfg.Assets.MarkFailed(ctx, job.AssetID, detail); uerr != nil {
		log.Error("could not update the asset", "error", uerr)
	}
}

// heartbeat renews the lease until the returned function is called. Losing the
// lease cancels the job.
func (p *Pool) heartbeat(ctx context.Context, cancelJob context.CancelFunc, log *slog.Logger, worker string, jobID int64) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		every := p.cfg.Lease / 3
		if every < time.Second {
			every = time.Second
		}
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Not jobCtx: a renewal must still be attempted while the job
				// is being torn down, and a cancelled context cannot query.
				beatCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				err := p.cfg.Queue.Heartbeat(beatCtx, jobID, worker, p.cfg.Lease)
				cancel()
				if err != nil {
					log.Error("lost the lease on this job; stopping work on it", "error", err)
					cancelJob()
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func (p *Pool) reclaim(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Lease / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			moved, err := p.cfg.Queue.Reclaim(ctx)
			if err != nil {
				if ctx.Err() == nil {
					p.cfg.Log.Error("reclaim failed", "error", err)
				}
				continue
			}
			if moved > 0 {
				p.cfg.Log.Warn("took back jobs whose worker stopped renewing them", "jobs", moved)
			}
		}
	}
}

func (p *Pool) execute(ctx context.Context, log *slog.Logger, job queue.Job) error {
	switch job.Kind {
	case queue.KindTranscode:
		return p.transcode(ctx, log, job)
	default:
		return fmt.Errorf("no worker in this build knows how to run a %q job", job.Kind)
	}
}

// transcode is the pipeline: source out of the store, probe, ladder, encode,
// frames, poster, everything into the store, address published.
//
// There are two ways through the encode. The newer one runs a single ffmpeg
// that writes the ladder and samples the frames from one decode, publishes
// segments while it runs, and reports progress. The older one is what 0.1.0
// did, kept exactly. The newer is tried first and the older is the fallback,
// automatically and for any reason: nothing 0.2.0 added may be a way for an
// encode to fail that would have succeeded before it.
func (p *Pool) transcode(ctx context.Context, log *slog.Logger, job queue.Job) error {
	asset, err := p.cfg.Assets.Get(ctx, job.AssetID)
	if err != nil {
		return err
	}
	if err := p.cfg.Assets.MarkProcessing(ctx, asset.ID); err != nil {
		return err
	}

	dir, err := os.MkdirTemp(p.cfg.WorkDir, "job-*")
	if err != nil {
		return fmt.Errorf("work directory: %w", err)
	}
	defer os.RemoveAll(dir)

	source := filepath.Join(dir, "source")
	if err := p.fetch(ctx, asset.SourceKey, source); err != nil {
		return err
	}

	probed, err := p.cfg.Runner.Probe(ctx, source)
	if err != nil {
		return err
	}

	prefix := path.Join("out", asset.ID)
	// An earlier attempt may have published part of a package before it died.
	// This attempt starts from nothing rather than writing over some of it.
	p.clearTree(ctx, log, prefix)

	if !p.cfg.Legacy {
		err := p.encodeProgressive(ctx, log, asset, source, filepath.Join(dir, "new"), probed, prefix)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// Stopped, not failed: the node is shutting down or the lease was
			// lost. Encoding again from the top is the opposite of stopping.
			return err
		}
		log.Warn("the publish-as-it-goes encode failed; encoding this asset the way 0.1.0 did",
			"error", err, "encoder", p.cfg.Encoding.Encoder(), "rate_mode", string(p.cfg.Encoding.Mode()))
		// Whatever it published belongs to an encode that did not finish.
		if uerr := p.cfg.Assets.Unpublish(ctx, asset.ID); uerr != nil {
			return uerr
		}
		p.clearTree(ctx, log, prefix)
	}
	return p.encodeLegacy(ctx, log, asset, source, filepath.Join(dir, "legacy"), probed, prefix)
}

// clearTree removes a package prefix when the store can. When it cannot, the
// encode still runs and overwrites every key it writes; only stale extras
// could survive, and those are not referenced by any playlist.
func (p *Pool) clearTree(ctx context.Context, log *slog.Logger, prefix string) {
	deleter, ok := p.cfg.Store.(store.TreeDeleter)
	if !ok {
		return
	}
	if err := deleter.DeleteTree(ctx, prefix); err != nil {
		log.Warn("could not clear what an earlier attempt published; it will be overwritten instead", "error", err)
	}
}

// outputs is the asset's record of a ladder.
func outputs(prefix string, ladder []pipeline.Rendition, probed pipeline.Media) assets.Outputs {
	out := assets.Outputs{DurationMs: probed.DurationMs}
	for _, r := range ladder {
		out.Renditions = append(out.Renditions, assets.Rendition{
			Name:        r.Name,
			Height:      r.Height,
			Width:       pipeline.WidthFor(probed, r.Height),
			BitrateKbps: r.VideoKbps,
			Playlist:    path.Join(prefix, r.Name, "index.m3u8"),
		})
	}
	return out
}

// encodeProgressive is the 0.2.0 encode: one ffmpeg for the ladder and the
// frames, segments uploaded as they are closed, the address handed out as soon
// as it plays, progress written as it goes.
func (p *Pool) encodeProgressive(ctx context.Context, log *slog.Logger, asset assets.Asset, source, dir string, probed pipeline.Media, prefix string) error {
	enc := p.cfg.Encoding
	ladder := enc.Ladder(pipeline.Ladder(probed))
	log.Info("encoding", "duration_ms", probed.DurationMs, "source", fmt.Sprintf("%dx%d", probed.Width, probed.Height),
		"rungs", len(ladder), "encoder", enc.Encoder(), "rate_mode", string(enc.Mode()))

	outDir, framesDir := filepath.Join(dir, "out"), filepath.Join(dir, "frames")
	rungs := make([]string, 0, len(ladder))
	for _, r := range ladder {
		// ffmpeg expands %v but does not create directories.
		if err := os.MkdirAll(filepath.Join(outDir, r.Name), 0o755); err != nil {
			return fmt.Errorf("output directory: %w", err)
		}
		rungs = append(rungs, r.Name)
	}
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return fmt.Errorf("frames directory: %w", err)
	}

	args := pipeline.HLSJob{
		Input:       source,
		OutDir:      outDir,
		Ladder:      ladder,
		Source:      probed,
		Encoding:    enc,
		Progressive: true,
		FramesDir:   framesDir,
		FrameEvery:  p.cfg.FrameEvery,
		MaxFrames:   p.cfg.MaxFrames,
	}.Args()

	// ffmpeg reports from its own goroutine about once a second. The latest
	// report is kept here and written by the loop below, on its own schedule.
	var latest atomic.Uint64
	encodeCtx, stopEncode := context.WithCancel(ctx)
	defer stopEncode()
	proc, err := p.cfg.Runner.Start(encodeCtx, args, false, func(report pipeline.Progress) {
		if report.Done {
			// "Done" is ffmpeg having written its last frame. The package is
			// not verified and the asset is not ready, and only MarkReady may
			// say 1.
			return
		}
		latest.Store(math.Float64bits(report.Fraction(probed.DurationMs)))
	})
	if err != nil {
		return err
	}

	pub := &publish.Publisher{Store: p.cfg.Store, Dir: outDir, Prefix: prefix, Rungs: rungs}
	address := p.cfg.Store.Address(path.Join(prefix, pipeline.MasterPlaylist))
	published := false
	written, writtenAt := 0.0, time.Time{}

	tick := func() error {
		status, err := pub.Sync(ctx)
		if err != nil {
			return err
		}
		if status.Playable && !published {
			if err := p.cfg.Assets.MarkPublished(ctx, asset.ID, address, outputs(prefix, ladder, probed)); err != nil {
				return fmt.Errorf("publishing the address early: %w", err)
			}
			published = true
			log.Info("playable while still encoding", "address", address, "segments", status.Segments)
		}
		if fraction := math.Float64frombits(latest.Load()); fraction > written && time.Since(writtenAt) >= p.cfg.ProgressEvery {
			// Best effort: a progress number that could not be written is not
			// a reason to stop an encode that is going fine.
			if err := p.cfg.Assets.SetProgress(ctx, asset.ID, fraction); err != nil {
				log.Debug("could not record progress", "error", err)
			}
			written, writtenAt = fraction, time.Now()
		}
		return nil
	}

	ticker := time.NewTicker(p.cfg.SyncEvery)
	defer ticker.Stop()
running:
	for {
		select {
		case <-proc.Exited():
			break running
		case <-ticker.C:
			if err := tick(); err != nil {
				// The store or the database refused something. Stop ffmpeg
				// rather than let it encode an hour that cannot be published.
				stopEncode()
				_ = proc.Wait()
				return err
			}
		}
	}
	if err := proc.Wait(); err != nil {
		return err
	}

	result, err := pub.Finish(ctx, true)
	if err != nil {
		return err
	}
	// The two things the argv asked for and only the output can confirm.
	if err := pipeline.CheckAligned(result.Playlists); err != nil {
		return err
	}
	master, err := os.ReadFile(filepath.Join(outDir, pipeline.MasterPlaylist))
	if err != nil {
		return fmt.Errorf("ffmpeg finished but wrote no %s", pipeline.MasterPlaylist)
	}
	if err := pipeline.CheckMaster(master, ladder); err != nil {
		return err
	}

	// The frames came out of the same ffmpeg as the ladder. The poster is its
	// own call, as before: it seeks straight to one keyframe and decodes a
	// handful of frames, which is not the second full decode this path exists
	// to avoid.
	frameKeys, err := p.storeFrames(ctx, prefix, framesDir)
	if err != nil {
		return err
	}
	posterKey, err := p.poster(ctx, log, source, filepath.Join(dir, "poster.jpg"), prefix, probed)
	if err != nil {
		return err
	}

	out := outputs(prefix, ladder, probed)
	out.Frames, out.Poster = frameKeys, posterKey
	return p.cfg.Assets.MarkReady(ctx, asset.ID, address, out)
}

// poster takes the thumbnail and stores it. Best-effort on the ffmpeg side —
// a source with no decodable frame at that point still has a package — and
// strict on the store side, like every other write.
func (p *Pool) poster(ctx context.Context, log *slog.Logger, source, posterPath, prefix string, probed pipeline.Media) (string, error) {
	if err := p.cfg.Runner.Run(ctx, pipeline.PosterArgs(source, posterPath, pipeline.PosterAt(probed.DurationMs))); err != nil {
		log.Warn("no poster was taken from this source", "error", err)
		return "", nil
	}
	f, err := os.Open(posterPath)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	obj, err := p.cfg.Store.Put(ctx, path.Join(prefix, "poster.jpg"), f)
	if err != nil {
		return "", err
	}
	return obj.Key, nil
}

// encodeLegacy is the 0.1.0 encode, step for step: one ffmpeg for the ladder
// at libx264's fixed bitrates, a second for the frames, a third for the
// poster, the whole tree uploaded at the end, and the address published only
// then. It ignores the configured encoder on purpose — it is the path that is
// known to work, and it is only useful as a fallback while it stays that.
func (p *Pool) encodeLegacy(ctx context.Context, log *slog.Logger, asset assets.Asset, source, dir string, probed pipeline.Media, prefix string) error {
	ladder := pipeline.Ladder(probed)
	log.Info("encoding", "duration_ms", probed.DurationMs, "source", fmt.Sprintf("%dx%d", probed.Width, probed.Height), "rungs", len(ladder), "path", "legacy")

	outDir := filepath.Join(dir, "out")
	for _, r := range ladder {
		// ffmpeg expands %v but does not create directories.
		if err := os.MkdirAll(filepath.Join(outDir, r.Name), 0o755); err != nil {
			return fmt.Errorf("output directory: %w", err)
		}
	}
	if err := p.cfg.Runner.Run(ctx, pipeline.HLSArgs(source, outDir, ladder, probed)); err != nil {
		return err
	}
	master := filepath.Join(outDir, pipeline.MasterPlaylist)
	if _, err := os.Stat(master); err != nil {
		// ffmpeg exiting 0 without writing the master playlist would leave an
		// asset marked ready whose address 404s.
		return fmt.Errorf("ffmpeg finished but wrote no %s", pipeline.MasterPlaylist)
	}

	framesDir := filepath.Join(dir, "frames")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return fmt.Errorf("frames directory: %w", err)
	}
	// Frames and poster are best-effort: they feed capabilities that are not
	// built yet, and losing them must not cost an encode that succeeded.
	if err := p.cfg.Runner.Run(ctx, pipeline.FramesArgs(source, framesDir, p.cfg.FrameEvery, p.cfg.MaxFrames)); err != nil {
		log.Warn("no frames were sampled from this source", "error", err)
	}

	if _, err := p.cfg.Store.PutTree(ctx, prefix, outDir); err != nil {
		return err
	}
	frameKeys, err := p.storeFrames(ctx, prefix, framesDir)
	if err != nil {
		return err
	}
	posterKey, err := p.poster(ctx, log, source, filepath.Join(dir, "poster.jpg"), prefix, probed)
	if err != nil {
		return err
	}

	out := outputs(prefix, ladder, probed)
	out.Frames, out.Poster = frameKeys, posterKey
	address := p.cfg.Store.Address(path.Join(prefix, pipeline.MasterPlaylist))
	return p.cfg.Assets.MarkReady(ctx, asset.ID, address, out)
}

func (p *Pool) fetch(ctx context.Context, key, dest string) error {
	src, _, err := p.cfg.Store.Open(ctx, key)
	if err != nil {
		return fmt.Errorf("source %s: %w", key, err)
	}
	defer src.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, src); err != nil {
		return fmt.Errorf("copying the source out of the store: %w", err)
	}
	return f.Close()
}

func (p *Pool) storeFrames(ctx context.Context, prefix, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jpg") {
			names = append(names, e.Name())
		}
	}
	// Frame order is time order, and a consumer sampling keyframes for a model
	// needs them in the order they happened.
	sort.Strings(names)
	keys := make([]string, 0, len(names))
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		obj, err := p.cfg.Store.Put(ctx, path.Join(prefix, "frames", name), f)
		f.Close()
		if err != nil {
			return nil, err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}
