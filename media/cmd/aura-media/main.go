// Command aura-media is the media subsystem: it takes an asset and returns an
// address, and it runs beside the kernel rather than inside it.
//
// Beside, not inside, for three reasons that are not style. Approving an effect
// is milliseconds and must never queue behind a core pinned for minutes. The
// bytes here are public-zone and the kernel is where PHI dictation is handled.
// And libav's CVEs must not be a reason to move the kernel's version — which is
// why this is a second artefact with its own VERSION file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"aura/media/internal/api"
	"aura/media/internal/assets"
	"aura/media/internal/config"
	"aura/media/internal/db"
	"aura/media/internal/ingest"
	"aura/media/internal/pipeline"
	"aura/media/internal/queue"
	"aura/media/internal/store"
	"aura/media/internal/worker"
)

// version is stamped at build time from media/VERSION:
//
//	go build -ldflags "-X main.version=$(cat VERSION)" ./cmd/aura-media
//
// Its own line, deliberately: a consumer pins this and the kernel separately,
// so one entry never quietly means two things.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aura-media: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("aura-media "+command, flag.ContinueOnError)
	cfg := config.Config{}
	fs.StringVar(&cfg.DSN, "dsn", env("AURA_MEDIA_DSN", ""), "Postgres DSN for the queue and the asset table")
	fs.StringVar(&cfg.Bind, "bind", env("AURA_MEDIA_BIND", "127.0.0.1:9090"), "address to serve on")
	fs.StringVar(&cfg.DataDir, "data", env("AURA_MEDIA_DATA", defaultData()), "data directory: objects, scratch space and the token")
	fs.StringVar(&cfg.BaseURL, "base-url", env("AURA_MEDIA_BASE_URL", ""), "public prefix for addresses, when something else fronts this node")
	fs.IntVar(&cfg.Workers, "workers", envInt("AURA_MEDIA_WORKERS", 2), "how many jobs run at once; this is the parallelism")
	fs.DurationVar(&cfg.Lease, "lease", 2*time.Minute, "how long a claimed job is held between heartbeats")
	fs.Int64Var(&cfg.MaxBytes, "max-bytes", int64(envInt("AURA_MEDIA_MAX_BYTES", 0)), "largest source accepted, in bytes (0 for the default 8GiB)")
	fs.StringVar(&cfg.FFmpeg, "ffmpeg", env("AURA_MEDIA_FFMPEG", "ffmpeg"), "ffmpeg binary")
	fs.StringVar(&cfg.FFprobe, "ffprobe", env("AURA_MEDIA_FFPROBE", "ffprobe"), "ffprobe binary")
	fs.BoolVar(&cfg.NoAuth, "no-auth", false, "serve the control surface with no token (loopback only)")
	fs.BoolVar(&cfg.FetchHTTP, "fetch-http", false, "allow ingesting a source named by an http(s) URI")
	fs.Float64Var(&cfg.FrameEvery, "frame-every", 5, "seconds between sampled still frames")
	fs.IntVar(&cfg.MaxFrames, "max-frames", 200, "most still frames to sample from one source")
	printSchema := fs.Bool("print", false, "migrate: print the DDL instead of applying it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch command {
	case "version":
		fmt.Printf("aura-media %s\n", version)
		return nil
	case "migrate":
		if *printSchema {
			fmt.Print(db.Schema())
			return nil
		}
		return migrate(cfg, log)
	case "serve":
		return serve(cfg, log)
	default:
		return fmt.Errorf("unknown command %q (serve, migrate, version)", command)
	}
}

func migrate(cfg config.Config, log *slog.Logger) error {
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DSN, 4)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	log.Info("schema is in place")
	return nil
}

func serve(cfg config.Config, log *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	runner := pipeline.Runner{FFmpeg: cfg.FFmpeg, FFprobe: cfg.FFprobe}
	// Loud and early: a node without ffmpeg can accept every upload and fail
	// every one of them, and the failure would land four minutes into a job
	// rather than in front of whoever started the process.
	if err := runner.Check(); err != nil {
		return err
	}
	token, err := config.ResolveToken(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DSN, int32(cfg.Workers)+4)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	objects, err := store.NewFS(cfg.ObjectRoot(), cfg.BaseURL, "/media")
	if err != nil {
		return err
	}
	repo := assets.Open(pool)
	jobs := queue.Open(pool)
	ingestor := &ingest.Ingestor{Assets: repo, Queue: jobs, Store: objects, MaxBytes: cfg.MaxBytes}
	if cfg.FetchHTTP {
		ingestor.HTTP = &http.Client{Timeout: 0}
	}

	pool2, err := worker.New(worker.Config{
		Queue:      jobs,
		Assets:     repo,
		Store:      objects,
		Runner:     runner,
		Log:        log,
		Count:      cfg.Workers,
		WorkDir:    cfg.WorkRoot(),
		Lease:      cfg.Lease,
		FrameEvery: cfg.FrameEvery,
		MaxFrames:  cfg.MaxFrames,
	})
	if err != nil {
		return err
	}

	server := &api.Server{
		Assets:      repo,
		Queue:       jobs,
		Store:       objects,
		Ingest:      ingestor,
		Log:         log,
		Version:     version,
		Token:       token,
		MediaPrefix: "/media",
		Ready: func() error {
			pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := pool.Ping(pingCtx); err != nil {
				return fmt.Errorf("the database is unreachable: %w", err)
			}
			return runner.Check()
		},
	}
	httpServer := &http.Server{
		Addr:              cfg.Bind,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: an upload of a two-hour recording and a playback of
		// a large segment are both legitimately slow, and a deadline here would
		// cut them off mid-body.
	}

	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		pool2.Run(ctx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("aura-media listening",
			"version", version, "bind", cfg.Bind, "workers", cfg.Workers,
			"data", cfg.DataDir, "auth", token != "")
		if token == "" {
			log.Warn("the control surface has no token; anything that can reach this port can queue work on it")
		}
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		// SIGTERM is what `docker stop`, a pod deletion and `systemctl stop`
		// all send, so this path runs on every deploy. Stop accepting, let the
		// workers put down what they are holding, and only then close the pool.
		log.Info("stopping: draining")
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Error("the HTTP server did not drain", "error", err)
		}
	}

	select {
	case <-workersDone:
	case <-time.After(60 * time.Second):
		log.Error("workers did not stop in time; their jobs will be reclaimed when their leases expire")
	}
	log.Info("stopped cleanly")
	return nil
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}

func defaultData() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aura-media"
	}
	return filepath.Join(home, ".aura-media")
}
