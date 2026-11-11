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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"aura/media"
	"aura/media/internal/api"
	"aura/media/internal/assets"
	"aura/media/internal/c3"
	"aura/media/internal/config"
	"aura/media/internal/db"
	"aura/media/internal/ingest"
	"aura/media/internal/live"
	"aura/media/internal/pipeline"
	"aura/media/internal/queue"
	"aura/media/internal/rtmp"
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
	fs.StringVar(&cfg.NodeWS, "node", env("AURA_MEDIA_NODE", ""), "kernel to register with as a skill, e.g. ws://127.0.0.1:9080/ws/skill (empty: do not register)")
	fs.StringVar(&cfg.NodeToken, "node-token", "", "credential for that kernel (default: AURA_TOKEN, then ~/.aura/node.token)")
	fs.StringVar(&cfg.VideoEncoder, "video-encoder", env("AURA_MEDIA_VIDEO_ENCODER", pipeline.EncoderX264), "video encoder: "+strings.Join(pipeline.Encoders(), ", "))
	fs.StringVar(&cfg.Preset, "preset", env("AURA_MEDIA_PRESET", ""), "encoder preset (empty: this build's default for that encoder)")
	fs.StringVar(&cfg.RateMode, "rate-mode", env("AURA_MEDIA_RATE_MODE", string(pipeline.RateCBRLadder)), "cbr-ladder (a fixed bitrate per rung) or capped-crf (a quality per title, with the rung's bitrate as a ceiling)")
	fs.IntVar(&cfg.CRF, "crf", envInt("AURA_MEDIA_CRF", 0), "quality target for capped-crf (0: the encoder's customary default)")
	fs.BoolVar(&cfg.LegacyEncode, "legacy-encode", env("AURA_MEDIA_LEGACY_ENCODE", "") != "", "encode exactly as 0.1.0 did: publish only when finished, no progress, libx264 at fixed bitrates")
	fs.StringVar(&cfg.RTMPBind, "rtmp-bind", env("AURA_MEDIA_RTMP_BIND", ""), "address to accept live RTMP on, e.g. 0.0.0.0:1935 (empty: no live)")
	fs.StringVar(&cfg.RTMPURL, "rtmp-url", env("AURA_MEDIA_RTMP_URL", ""), "ingest URL broadcasters are given, without the key, e.g. rtmp://live.example.com/live")
	fs.DurationVar(&cfg.LiveGrace, "live-grace", 30*time.Second, "how long a broadcaster that dropped has to reconnect before the stream is closed")
	fs.DurationVar(&cfg.LiveWindow, "live-window", 2*time.Minute, "how much of a live stream its playlist lists (0: all of it); older segments are kept either way")
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
	encoding := cfg.Encoding()
	if !encoding.IsDefault() && !cfg.LegacyEncode {
		// The same reasoning as the check above, one level down: an encoder
		// this ffmpeg was not built with would fail every job at its first
		// frame, be rescued by the fallback every time, and look from the
		// outside like a node that was simply slow.
		//
		// Being built with an encoder is not the same as it working here, so
		// after asking ffmpeg whether it has it, a few seconds of test pattern
		// are actually encoded with it and the result is checked.
		checkCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := runner.HasEncoder(checkCtx, encoding.Encoder())
		if err == nil {
			err = runner.SelfTest(checkCtx, encoding, os.TempDir())
		}
		cancel()
		if err != nil {
			return err
		}
		log.Info("the configured encoder passed its self-test", "encoder", encoding.Encoder(), "rate_mode", string(encoding.Mode()))
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
		Encoding:   encoding,
		Legacy:     cfg.LegacyEncode,
	})
	if err != nil {
		return err
	}

	// Live is off unless there is somewhere for a broadcaster to connect.
	// When it is on, the listener is opened here, before anything is served:
	// a port that is already taken is a start-up failure, not something to
	// discover when the first stream is refused.
	var streams *live.Manager
	liveDone := make(chan struct{})
	if cfg.RTMPBind != "" {
		streams, err = live.New(live.Config{
			Repo:       live.OpenRepo(pool),
			Assets:     repo,
			Store:      objects,
			Runner:     runner,
			Log:        log,
			WorkDir:    cfg.LiveRoot(),
			IngestURL:  cfg.IngestURL(),
			Encoding:   encoding,
			Grace:      cfg.LiveGrace,
			Window:     cfg.LiveWindow,
			FrameEvery: cfg.FrameEvery,
			MaxFrames:  cfg.MaxFrames,
		})
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", cfg.RTMPBind)
		if err != nil {
			return fmt.Errorf("--rtmp-bind %s: %w", cfg.RTMPBind, err)
		}
		ingestServer := &rtmp.Server{Handler: streams, Log: log}
		go func() {
			if err := ingestServer.Serve(ctx, listener); err != nil {
				log.Error("the RTMP listener stopped", "error", err)
			}
		}()
		go func() {
			defer close(liveDone)
			streams.Run(ctx)
		}()
		log.Info("accepting live streams", "rtmp", cfg.RTMPBind, "ingest_url", cfg.IngestURL(),
			"grace", cfg.LiveGrace.String(), "window", cfg.LiveWindow.String())
	} else {
		close(liveDone)
	}

	server := &api.Server{
		Assets:      repo,
		Queue:       jobs,
		Store:       objects,
		Ingest:      ingestor,
		Log:         log,
		Version:     version,
		Live:        streams,
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

	// Registering is optional. The service is complete without it — upload,
	// poll, fetch an address — and a kernel that is not running must not stop
	// it from doing that work for the consumers that call it directly.
	if cfg.NodeWS != "" {
		manifest, err := c3.ManifestJSON(media.SkillYAML, version)
		if err != nil {
			return err
		}
		client := &c3.Client{
			URL:      cfg.NodeWS,
			Token:    config.ResolveNodeToken(cfg.NodeToken),
			Manifest: manifest,
			Ingest:   ingestor,
			Assets:   repo,
			Store:    objects,
			Log:      log,
		}
		go client.Run(ctx)
		log.Info("registering with the kernel as a skill", "node", cfg.NodeWS, "capability", "logical.media.transcode")
	}

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
	// Live streams are closed into VODs on the way out, which means waiting
	// for ffmpeg to flush and for the last segments to upload. Longer than the
	// workers get, because a job handed back is encoded again by someone else
	// and a broadcast is not.
	select {
	case <-liveDone:
	case <-time.After(2 * time.Minute):
		log.Error("live streams did not close in time; another start will close what they published")
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
