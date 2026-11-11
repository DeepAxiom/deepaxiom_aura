package worker_test

// The worker against a real Postgres and a fake ffmpeg (see
// internal/fakeffmpeg): the test binary re-executes itself as "ffmpeg", reads
// the argv the real builders produced and writes what ffmpeg would have
// written. What is under test is everything around the encode — when the
// address appears, what progress says, and what happens when the new path
// fails — and none of that should need a codec to check, or be uncheckable
// because a real ffmpeg cannot be made to fail on cue.
//
// Set AURA_MEDIA_TEST_DSN to run them.

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aura/media/internal/assets"
	"aura/media/internal/fakeffmpeg"
	"aura/media/internal/id"
	"aura/media/internal/pipeline"
	"aura/media/internal/queue"
	"aura/media/internal/store"
	"aura/media/internal/testdb"
	"aura/media/internal/worker"
)

func TestMain(m *testing.M) {
	fakeffmpeg.Main() // does not return when this process is the fake
	os.Exit(m.Run())
}

type rig struct {
	t       *testing.T
	repo    *assets.Repo
	jobs    *queue.Queue
	objects *store.FS
	pool    *worker.Pool
	argvLog string
	logs    *lockedBuffer
	stop    context.CancelFunc
	stopped chan struct{}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newRig wires a one-worker pool whose ffmpeg is this test binary in the given
// fault mode.
func newRig(t *testing.T, mode string, tweak func(*worker.Config)) *rig {
	t.Helper()
	db := testdb.Pool(t)
	objects, err := store.NewFS(t.TempDir(), "", "/media")
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv(fakeffmpeg.EnvMode, mode)
	t.Setenv(fakeffmpeg.EnvLog, argvLog)
	t.Setenv(fakeffmpeg.EnvDelayMs, "120")

	r := &rig{
		t:       t,
		repo:    assets.Open(db),
		jobs:    queue.Open(db),
		objects: objects,
		argvLog: argvLog,
		logs:    &lockedBuffer{},
		stopped: make(chan struct{}),
	}
	cfg := worker.Config{
		Queue:         r.jobs,
		Assets:        r.repo,
		Store:         objects,
		Runner:        pipeline.Runner{FFmpeg: self, FFprobe: self},
		Log:           slog.New(slog.NewTextHandler(r.logs, nil)),
		Count:         1,
		WorkDir:       t.TempDir(),
		Lease:         30 * time.Second,
		Idle:          20 * time.Millisecond,
		SyncEvery:     15 * time.Millisecond,
		ProgressEvery: time.Millisecond,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	if r.pool, err = worker.New(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	go func() {
		defer close(r.stopped)
		r.pool.Run(ctx)
	}()
	t.Cleanup(r.shutdown)
	return r
}

func (r *rig) shutdown() {
	r.stop()
	select {
	case <-r.stopped:
	case <-time.After(20 * time.Second):
		r.t.Error("the worker pool did not stop")
	}
}

// submit stores a source and queues it, as an upload does.
func (r *rig) submit() string {
	r.t.Helper()
	ctx := context.Background()
	assetID := id.New()
	if _, err := r.objects.Put(ctx, "src/"+assetID, strings.NewReader("not really a video")); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.repo.Create(ctx, assets.Asset{ID: assetID, SourceKey: "src/" + assetID, SourceSHA256: assetID}); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.jobs.Enqueue(ctx, queue.New{AssetID: assetID, Kind: queue.KindTranscode}); err != nil {
		r.t.Fatal(err)
	}
	return assetID
}

// watch polls the asset until done says stop, and returns everything it saw.
func (r *rig) watch(assetID string, done func(assets.Asset) bool) []assets.Asset {
	r.t.Helper()
	var seen []assets.Asset
	deadline := time.Now().Add(30 * time.Second)
	for {
		a, err := r.repo.Get(context.Background(), assetID)
		if err != nil {
			r.t.Fatal(err)
		}
		seen = append(seen, a)
		if done(a) {
			return seen
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("gave up waiting; the asset is %+v\n%s", a, r.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func isReady(a assets.Asset) bool { return a.State == assets.StateReady }

// job returns the asset's one job once it has reached the wanted state. The
// asset is marked before the job is closed, so a test that has just seen the
// asset change has to wait a moment for the job to follow.
func (r *rig) job(assetID string, want queue.State) queue.Job {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobs, err := r.jobs.ForAsset(context.Background(), assetID)
		if err != nil {
			r.t.Fatal(err)
		}
		if len(jobs) != 1 {
			r.t.Fatalf("the asset has %d jobs, want 1: %+v", len(jobs), jobs)
		}
		if jobs[0].State == want {
			return jobs[0]
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the job is %+v, want state %q", jobs[0], want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// calls is every ffmpeg/ffprobe invocation, classified.
func (r *rig) calls() []string {
	r.t.Helper()
	raw, err := os.ReadFile(r.argvLog)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		args := strings.Split(line, "\t")
		has := func(s string) bool {
			for _, a := range args {
				if a == s {
					return true
				}
			}
			return false
		}
		last := args[len(args)-1]
		switch {
		case has("-show_format"):
			out = append(out, "probe")
		case has("hls") && has("-progress"):
			kind := "ladder+progress"
			if strings.HasSuffix(last, "frame_%05d.jpg") {
				kind += "+frames"
			}
			out = append(out, kind)
		case has("hls"):
			out = append(out, "ladder-legacy")
		case strings.HasSuffix(last, "frame_%05d.jpg"):
			out = append(out, "frames-legacy")
		case strings.HasSuffix(last, "poster.jpg"):
			out = append(out, "poster")
		default:
			out = append(out, "other")
		}
	}
	return out
}

// tree lists what is in the store under an asset's package.
func (r *rig) tree(assetID string) []string {
	r.t.Helper()
	root := filepath.Join(r.objects.Root, "out", assetID)
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (r *rig) object(key string) string {
	r.t.Helper()
	path, err := r.objects.Path(key)
	if err != nil {
		r.t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

func TestAnAssetIsPlayableWhileItIsStillEncodingAndReadyOnlyWhenComplete(t *testing.T) {
	r := newRig(t, "ok", nil)
	assetID := r.submit()

	var sawEarly bool
	history := r.watch(assetID, func(a assets.Asset) bool {
		if a.State == assets.StateProcessing && a.Address != "" && !sawEarly {
			sawEarly = true
			// Checked at the moment it is first seen, not afterwards: the
			// address must already serve, and what it serves must already be
			// consistent, while the encode is still running.
			if !a.Growing {
				t.Errorf("an early address without growing=true: %+v", a)
			}
			master := r.object("out/" + assetID + "/master.m3u8")
			if !strings.Contains(master, "720p/index.m3u8") {
				t.Errorf("the address was handed out before the master playlist was in the store: %q", master)
			}
			for _, rendition := range a.Outputs.Renditions {
				playlist, err := pipeline.ParseMediaPlaylist([]byte(r.object(rendition.Playlist)))
				if err != nil || len(playlist.Segments) == 0 {
					t.Errorf("rung %s has nothing to play at the moment the address appeared (%v)", rendition.Name, err)
					continue
				}
				for _, file := range playlist.Files() {
					if r.object(filepath.ToSlash(filepath.Join(filepath.Dir(rendition.Playlist), file))) == "" {
						t.Errorf("rung %s lists %s, which is not in the store", rendition.Name, file)
					}
				}
			}
			if len(a.Outputs.Renditions) != 3 {
				t.Errorf("the ladder is not in the record while it grows: %+v", a.Outputs)
			}
		}
		return isReady(a)
	})
	if !sawEarly {
		t.Fatalf("the address never appeared before the asset was ready\n%s", r.logs.String())
	}

	// Progress: somewhere in the middle at least once, never backwards, 1 at
	// the end and not before.
	last, middle := 0.0, 0
	for i, a := range history {
		if a.Progress < last {
			t.Errorf("progress went from %v to %v", last, a.Progress)
		}
		last = a.Progress
		if a.Progress > 0 && a.Progress < 1 {
			middle++
		}
		if a.Progress == 1 && a.State != assets.StateReady {
			t.Errorf("snapshot %d says progress 1 in state %q; only a ready asset is finished", i, a.State)
		}
	}
	if middle == 0 {
		t.Error("progress was never reported between 0 and 1")
	}

	final := history[len(history)-1]
	if final.Growing || final.Progress != 1 || final.Address != "/media/out/"+assetID+"/master.m3u8" {
		t.Errorf("the ready asset is %+v", final)
	}
	if len(final.Outputs.Frames) != 3 || final.Outputs.Poster == "" || final.DurationMs != 16000 {
		t.Errorf("the ready asset's outputs are %+v", final.Outputs)
	}
	// Closed: every rung's playlist in the store ends with ENDLIST.
	for _, rendition := range final.Outputs.Renditions {
		playlist, err := pipeline.ParseMediaPlaylist([]byte(r.object(rendition.Playlist)))
		if err != nil || !playlist.Ended || len(playlist.Segments) != 4 {
			t.Errorf("rung %s in the store: ended=%v, %d segments (%v)", rendition.Name, playlist.Ended, len(playlist.Segments), err)
		}
	}

	// One decode. The frames came from the ladder's invocation, and the old
	// second pass over the source did not run.
	got := strings.Join(r.calls(), " ")
	if got != "probe ladder+progress+frames poster" {
		t.Errorf("ffmpeg was run as [%s], want [probe ladder+progress+frames poster]", got)
	}
	r.job(assetID, queue.StateDone)
}

func TestWhenTheNewEncodeFailsTheOldOneRunsAndNothingOfTheFailureIsLeft(t *testing.T) {
	for _, mode := range []string{fakeffmpeg.FailProgressive, fakeffmpeg.Misaligned, fakeffmpeg.NoCodecs} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, mode, nil)
			assetID := r.submit()

			var published, withdrawn bool
			history := r.watch(assetID, func(a assets.Asset) bool {
				if a.State == assets.StateProcessing && a.Address != "" {
					published = true
				}
				if published && a.State == assets.StateProcessing && a.Address == "" {
					withdrawn = true
					if a.Growing {
						t.Errorf("no address and still growing: %+v", a)
					}
				}
				return isReady(a)
			})
			final := history[len(history)-1]

			// The job did not fail. That is the whole point: 0.1.0 would have
			// encoded this source, so 0.2.0 must.
			if job := r.job(assetID, queue.StateDone); job.Attempts != 1 {
				t.Fatalf("the job is %+v; a failure of the new path must not cost an attempt", job)
			}
			calls := strings.Join(r.calls(), " ")
			if calls != "probe ladder+progress+frames ladder-legacy frames-legacy poster" {
				t.Errorf("ffmpeg was run as [%s]", calls)
			}
			if !strings.Contains(r.logs.String(), "encoding this asset the way 0.1.0 did") {
				t.Errorf("the fallback was not logged:\n%s", r.logs.String())
			}

			// What the failed attempt published was taken back before the
			// legacy encode's package replaced it.
			if !published {
				t.Fatal("the test did not exercise an early publish; the failure came too soon")
			}
			if !withdrawn {
				t.Error("the address the failed attempt published was never withdrawn")
			}
			if final.Growing || final.Progress != 1 || final.Address == "" || len(final.Outputs.Frames) != 3 || final.Outputs.Poster == "" {
				t.Errorf("the asset the fallback produced is %+v", final)
			}

			// Nothing of the failed attempt is in the store. The legacy path
			// writes VOD playlists; anything else is a leftover.
			for _, rendition := range final.Outputs.Renditions {
				text := r.object(rendition.Playlist)
				if !strings.Contains(text, "#EXT-X-PLAYLIST-TYPE:VOD") || !strings.Contains(text, "#EXT-X-ENDLIST") {
					t.Errorf("rung %s in the store is not the legacy encode's playlist:\n%s", rendition.Name, text)
				}
			}
			for _, file := range r.tree(assetID) {
				if body := r.object("out/" + assetID + "/" + file); strings.Contains(body, "from the attempt that failed") {
					t.Errorf("%s survived from the attempt that failed", file)
				}
			}
			if mode == fakeffmpeg.Misaligned {
				// The reason must be the one that says what was wrong.
				if !strings.Contains(r.logs.String(), "not aligned") {
					t.Errorf("a misaligned ladder was rejected without saying so:\n%s", r.logs.String())
				}
			}
			if mode == fakeffmpeg.NoCodecs && !strings.Contains(r.logs.String(), "CODECS") {
				t.Errorf("a master playlist with no CODECS was rejected without saying so:\n%s", r.logs.String())
			}
		})
	}
}

func TestLegacyModeIsExactlyTheOldBehaviour(t *testing.T) {
	r := newRig(t, "ok", func(c *worker.Config) {
		c.Legacy = true
		// Even with another encoder configured: legacy means the 0.1.0 encode.
		c.Encoding = pipeline.Encoding{VideoEncoder: pipeline.EncoderX265, RateMode: pipeline.RateCappedCRF}
	})
	assetID := r.submit()
	history := r.watch(assetID, isReady)
	for _, a := range history[:len(history)-1] {
		if a.Address != "" || a.Growing || a.Progress != 0 {
			t.Fatalf("legacy mode showed an address, growth or progress before ready: %+v", a)
		}
	}
	if calls := strings.Join(r.calls(), " "); calls != "probe ladder-legacy frames-legacy poster" {
		t.Errorf("ffmpeg was run as [%s]", calls)
	}
	raw, _ := os.ReadFile(r.argvLog)
	if strings.Contains(string(raw), "libx265") || strings.Contains(string(raw), "-crf") {
		t.Errorf("legacy mode used the configured encoder:\n%s", raw)
	}
	final := history[len(history)-1]
	if final.Progress != 1 || len(final.Outputs.Frames) != 3 || final.Outputs.Poster == "" {
		t.Errorf("the legacy asset is %+v", final)
	}
}

func TestTheConfiguredEncoderReachesFfmpeg(t *testing.T) {
	r := newRig(t, "ok", func(c *worker.Config) {
		c.Encoding = pipeline.Encoding{VideoEncoder: pipeline.EncoderX265, RateMode: pipeline.RateCappedCRF, CRF: 24}
	})
	final := r.watch(r.submit(), isReady)
	raw, _ := os.ReadFile(r.argvLog)
	for _, want := range []string{"libx265", "-crf\t24", "hvc1"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the encode argv has no %q:\n%s", want, raw)
		}
	}
	// The record's bitrate is the codec's ladder, not H.264's.
	if got := final[len(final)-1].Outputs.Renditions[0].BitrateKbps; got != 2800*65/100 {
		t.Errorf("the 720p rung is recorded at %dk", got)
	}
}

func TestWhenBothEncodesFailTheJobIsRetriedWithNothingPublished(t *testing.T) {
	r := newRig(t, fakeffmpeg.FailAll, nil)
	assetID := r.submit()
	history := r.watch(assetID, func(a assets.Asset) bool { return a.State == assets.StateReceived && a.Error != "" })
	final := history[len(history)-1]
	if final.Address != "" || final.Growing {
		t.Errorf("a failed attempt left the asset published: %+v", final)
	}
	if !strings.Contains(final.Error, "every encode fails") {
		t.Errorf("the asset's error is not ffmpeg's: %q", final.Error)
	}
	if job := r.job(assetID, queue.StateQueued); job.Attempts != 1 {
		t.Errorf("the job is %+v, want queued for a retry after one attempt", job)
	}
	// Both were tried, in order, once each.
	if calls := strings.Join(r.calls(), " "); calls != "probe ladder+progress+frames ladder-legacy" {
		t.Errorf("ffmpeg was run as [%s]", calls)
	}
	if files := r.tree(assetID); len(files) != 0 {
		t.Errorf("a failed attempt left %v in the store", files)
	}
}

func TestStoppingTheNodeMidEncodeHandsTheJobBackAndDoesNotStartTheFallback(t *testing.T) {
	t.Setenv(fakeffmpeg.EnvSegments, "200") // long enough to be interrupted
	r := newRig(t, "ok", nil)
	assetID := r.submit()
	r.watch(assetID, func(a assets.Asset) bool { return a.Growing })
	r.shutdown()

	if job := r.job(assetID, queue.StateQueued); job.Attempts != 0 {
		t.Errorf("the job is %+v, want handed back with no attempt spent", job)
	}
	// Being told to stop is not the new path failing. Starting a second,
	// slower encode of the same source on the way out would be the opposite
	// of stopping.
	if calls := strings.Join(r.calls(), " "); calls != "probe ladder+progress+frames" {
		t.Errorf("ffmpeg was run as [%s]", calls)
	}
	if strings.Contains(r.logs.String(), "the way 0.1.0 did") {
		t.Error("a shutdown was treated as a failure of the new encode")
	}
}

func TestASecondAttemptReplacesWhatTheFirstPublished(t *testing.T) {
	// Attempt one is interrupted after publishing. Attempt two — a new pool,
	// as after a deploy — must end with a package that is entirely its own.
	t.Setenv(fakeffmpeg.EnvSegments, "200")
	r := newRig(t, "ok", nil)
	assetID := r.submit()
	r.watch(assetID, func(a assets.Asset) bool { return a.Growing })
	r.shutdown()
	before := r.tree(assetID)
	if len(before) == 0 {
		t.Fatal("the interrupted attempt published nothing; the test proves nothing")
	}
	// Something the second attempt will not write, standing in for a rung or a
	// segment only the first one had.
	if _, err := r.objects.Put(context.Background(), "out/"+assetID+"/1080p/seg_00042.m4s", strings.NewReader("stale")); err != nil {
		t.Fatal(err)
	}

	t.Setenv(fakeffmpeg.EnvSegments, "3")
	// A new pool on the same database and the same store as the first.
	cfg := worker.Config{
		Queue: r.jobs, Assets: r.repo, Store: r.objects,
		Runner: pipeline.Runner{FFmpeg: os.Args[0], FFprobe: os.Args[0]},
		Log:    slog.New(slog.NewTextHandler(r.logs, nil)),
		Count:  1, WorkDir: t.TempDir(), Idle: 20 * time.Millisecond, SyncEvery: 15 * time.Millisecond,
	}
	pool, err := worker.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pool.Run(ctx) }()
	defer func() { cancel(); <-done }()

	final := r.watch(assetID, isReady)
	a := final[len(final)-1]
	for _, file := range r.tree(assetID) {
		if strings.Contains(file, "1080p") {
			t.Errorf("%s survived from the first attempt", file)
		}
	}
	for _, rendition := range a.Outputs.Renditions {
		playlist, err := pipeline.ParseMediaPlaylist([]byte(r.object(rendition.Playlist)))
		if err != nil || !playlist.Ended || len(playlist.Segments) != 3 {
			t.Errorf("rung %s after the second attempt: ended=%v, %d segments, want its own 3 (%v)", rendition.Name, playlist.Ended, len(playlist.Segments), err)
		}
	}
	// No segment beyond the second attempt's three is left in the store.
	for _, file := range r.tree(assetID) {
		if strings.Contains(file, "seg_") && !strings.HasSuffix(file, "seg_00000.m4s") && !strings.HasSuffix(file, "seg_00001.m4s") && !strings.HasSuffix(file, "seg_00002.m4s") {
			t.Errorf("%s is in the store and in no playlist", file)
		}
	}
}
