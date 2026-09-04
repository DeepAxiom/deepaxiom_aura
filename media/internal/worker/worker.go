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
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aura/media/internal/assets"
	"aura/media/internal/id"
	"aura/media/internal/pipeline"
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

// transcode is the pipeline: source out of the store, probe, ladder, one ffmpeg
// call, frames, poster, everything back into the store, address published.
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
	ladder := pipeline.Ladder(probed)
	log.Info("encoding", "duration_ms", probed.DurationMs, "source", fmt.Sprintf("%dx%d", probed.Width, probed.Height), "rungs", len(ladder))

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
	posterPath := filepath.Join(dir, "poster.jpg")
	posterOK := true
	if err := p.cfg.Runner.Run(ctx, pipeline.PosterArgs(source, posterPath, pipeline.PosterAt(probed.DurationMs))); err != nil {
		log.Warn("no poster was taken from this source", "error", err)
		posterOK = false
	}

	prefix := path.Join("out", asset.ID)
	if _, err := p.cfg.Store.PutTree(ctx, prefix, outDir); err != nil {
		return err
	}
	frameKeys, err := p.storeFrames(ctx, prefix, framesDir)
	if err != nil {
		return err
	}
	posterKey := ""
	if posterOK {
		f, err := os.Open(posterPath)
		if err == nil {
			obj, perr := p.cfg.Store.Put(ctx, path.Join(prefix, "poster.jpg"), f)
			f.Close()
			if perr != nil {
				return perr
			}
			posterKey = obj.Key
		}
	}

	out := assets.Outputs{
		DurationMs: probed.DurationMs,
		Frames:     frameKeys,
		Poster:     posterKey,
	}
	for _, r := range ladder {
		out.Renditions = append(out.Renditions, assets.Rendition{
			Name:        r.Name,
			Height:      r.Height,
			Width:       pipeline.WidthFor(probed, r.Height),
			BitrateKbps: r.VideoKbps,
			Playlist:    path.Join(prefix, r.Name, "index.m3u8"),
		})
	}
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
