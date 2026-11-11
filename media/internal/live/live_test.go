package live

// The stream's life against a real Postgres and a fake ffmpeg (see
// internal/fakeffmpeg). The tests stand where the RTMP server stands: they
// call Publish with a key and feed tags to the sink they are given. The fake
// reads the FLV the session writes to its stdin, refuses it if it does not
// begin with an FLV header or if time runs backwards in it, and closes a
// segment for every two seconds of video — so what these tests see published
// is a function of exactly what the session handed the encoder.
//
// Set AURA_MEDIA_TEST_DSN to run them.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aura/media/internal/assets"
	"aura/media/internal/fakeffmpeg"
	"aura/media/internal/pipeline"
	"aura/media/internal/publish"
	"aura/media/internal/rtmp"
	"aura/media/internal/store"
	"aura/media/internal/testdb"
)

func TestMain(m *testing.M) {
	fakeffmpeg.Main() // does not return when this process is the fake
	os.Exit(m.Run())
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

type rig struct {
	t       *testing.T
	m       *Manager
	repo    *Repo
	assets  *assets.Repo
	objects *store.FS
	logs    *lockedBuffer
	argvLog string
}

var remote = &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 50000}

func newRig(t *testing.T, mode string, tweak func(*Config)) *rig {
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
	r := &rig{
		t:       t,
		repo:    OpenRepo(db),
		assets:  assets.Open(db),
		objects: objects,
		logs:    &lockedBuffer{},
		argvLog: filepath.Join(t.TempDir(), "argv.log"),
	}
	t.Setenv(fakeffmpeg.EnvMode, mode)
	t.Setenv(fakeffmpeg.EnvLog, r.argvLog)
	cfg := Config{
		Repo:       r.repo,
		Assets:     r.assets,
		Store:      objects,
		Runner:     pipeline.Runner{FFmpeg: self, FFprobe: self},
		Log:        slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		WorkDir:    t.TempDir(),
		IngestURL:  "rtmp://ingest.example/live",
		Grace:      10 * time.Second,
		Window:     6 * time.Second, // three segments
		SyncEvery:  10 * time.Millisecond,
		ProbeAfter: time.Second,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	if r.m, err = New(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// End whatever a test left running, and wait for it: a session that
		// outlived its test would be writing into a schema that is gone.
		r.m.mu.Lock()
		r.m.stop()
		r.m.mu.Unlock()
		done := make(chan struct{})
		go func() { r.m.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("a live session did not end when the manager stopped")
		}
	})
	return r
}

func (r *rig) create() View {
	r.t.Helper()
	v, err := r.m.Create(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return v
}

func (r *rig) publish(key string) rtmp.Sink {
	r.t.Helper()
	sink, err := r.m.Publish(context.Background(), App, key, remote)
	if err != nil {
		r.t.Fatalf("publish refused: %v\n%s", err, r.logs.String())
	}
	return sink
}

// broadcast feeds a connection's worth of stream: the opening an encoder
// sends, then a keyframe and an audio packet every 100ms of stream time. The
// timestamps start at zero, as they do on every new RTMP connection.
func broadcast(t *testing.T, sink rtmp.Sink, milliseconds int) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("the session refused a tag: %v", err)
		}
	}
	must(sink.Tag(rtmp.TagScript, 0, []byte{2, 0, 10, 'o', 'n', 'M', 'e', 't', 'a', 'D', 'a', 't', 'a', 5}))
	must(sink.Tag(rtmp.TagVideo, 0, []byte{0x17, 0, 0, 0, 0, 1, 2, 3})) // AVC sequence header
	must(sink.Tag(rtmp.TagAudio, 0, []byte{0xaf, 0, 0x12, 0x10}))       // AAC sequence header
	for ts := 0; ts <= milliseconds; ts += 100 {
		must(sink.Tag(rtmp.TagVideo, uint32(ts), []byte{0x17, 1, 0, 0, 0, 0xde, 0xad}))
		must(sink.Tag(rtmp.TagAudio, uint32(ts), []byte{0xaf, 1, 0xbe, 0xef}))
	}
}

func (r *rig) waitAsset(assetID string, done func(assets.Asset) bool) assets.Asset {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		a, err := r.assets.Get(context.Background(), assetID)
		if err == nil && done(a) {
			return a
		}
		if err != nil && !errors.Is(err, assets.ErrNotFound) {
			r.t.Fatal(err)
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("gave up waiting; the asset is %+v (%v)\n%s", a, err, r.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) waitStream(streamID string, want State) Stream {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err := r.repo.Get(context.Background(), streamID)
		if err != nil {
			r.t.Fatal(err)
		}
		if s.State == want {
			return s
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the stream is %q, want %q\n%s", s.State, want, r.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) playlist(key string) (pipeline.MediaPlaylist, string) {
	r.t.Helper()
	body, _, err := r.objects.Open(context.Background(), key)
	if err != nil {
		r.t.Fatalf("%s: %v", key, err)
	}
	defer body.Close()
	raw, _ := io.ReadAll(body)
	pl, err := pipeline.ParseMediaPlaylist(raw)
	if err != nil {
		r.t.Fatalf("%s: %v", key, err)
	}
	return pl, string(raw)
}

func (r *rig) exists(key string) bool {
	body, _, err := r.objects.Open(context.Background(), key)
	if err != nil {
		return false
	}
	body.Close()
	return true
}

// segments waits until the published 720p playlist's live edge is segment n-1.
func (r *rig) segments(assetID string, n int) {
	r.t.Helper()
	key := "out/" + assetID + "/720p/dvr.m3u8"
	deadline := time.Now().Add(30 * time.Second)
	for {
		if r.exists(key) {
			if pl, _ := r.playlist(key); len(pl.Segments) >= n {
				return
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%d segments were never published\n%s", n, r.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func growing(a assets.Asset) bool { return a.Growing && a.Address != "" }
func ready(a assets.Asset) bool   { return a.State == assets.StateReady }
func ended(a assets.Asset) bool {
	return a.State == assets.StateReady || a.State == assets.StateFailed
}

func TestAStreamInAnAddressOut(t *testing.T) {
	r := newRig(t, "ok", nil)
	v := r.create()
	if v.State != StateIdle || v.StreamKey == "" || v.IngestURL != "rtmp://ingest.example/live" {
		t.Fatalf("a new stream is %+v", v)
	}
	// The address is known before anyone has broadcast, and nothing is there.
	if v.Address != "/media/out/"+v.AssetID+"/master.m3u8" {
		t.Errorf("address %q", v.Address)
	}
	if _, err := r.assets.Get(context.Background(), v.AssetID); !errors.Is(err, assets.ErrNotFound) {
		t.Errorf("a stream nobody has started already has an asset row (%v)", err)
	}
	// Reading it back never shows the key again.
	if again, _ := r.m.Get(context.Background(), v.ID); again.StreamKey != "" {
		t.Error("the key was returned a second time")
	}

	sink := r.publish(v.StreamKey)
	broadcast(t, sink, 9500) // four whole two-second segments and a bit

	live := r.waitAsset(v.AssetID, growing)
	if live.State != assets.StateProcessing || live.Kind != assets.KindLive || live.Address != v.Address {
		t.Errorf("the asset while live is %+v", live)
	}
	if len(live.Outputs.Renditions) != 3 {
		t.Errorf("the live ladder is %+v", live.Outputs.Renditions)
	}
	if got := r.waitStream(v.ID, StateLive); got.StartedAt == nil {
		t.Error("a live stream has no start time")
	}
	r.segments(v.AssetID, 4)

	// What a viewer polls: the last three segments, not closed, and the
	// sequence number saying one has slid off the front. The whole-stream
	// playlist is written a moment before the window, so wait for the window
	// to show the same live edge.
	indexKey := "out/" + v.AssetID + "/720p/index.m3u8"
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if pl, _ := r.playlist(indexKey); len(pl.Segments) > 0 && pl.Segments[len(pl.Segments)-1].URI == "seg_00003.m4s" {
			break
		}
	}
	window, text := r.playlist(indexKey)
	if len(window.Segments) != 3 || window.MediaSequence != 1 || window.Ended || window.TargetDuration != 2 {
		t.Errorf("the live playlist: %d segments from sequence %d, ended=%v, target %d\n%s",
			len(window.Segments), window.MediaSequence, window.Ended, window.TargetDuration, text)
	}
	// What slid out of the window is still stored.
	if !r.exists("out/" + v.AssetID + "/720p/seg_00000.m4s") {
		t.Error("the first segment left the store when it left the window")
	}

	// The broadcaster stops.
	sink.Close(nil)
	vod := r.waitAsset(v.AssetID, ready)
	if vod.Growing || vod.Address != v.Address || vod.Kind != assets.KindLive || vod.Progress != 1 {
		t.Errorf("the finished stream's asset is %+v", vod)
	}
	if vod.DurationMs != 8000 {
		t.Errorf("duration %dms, want the four segments' 8000", vod.DurationMs)
	}
	if len(vod.Outputs.Frames) != 3 || vod.Outputs.Poster == "" {
		t.Errorf("frames and poster of the stream: %+v", vod.Outputs)
	}
	for _, rendition := range vod.Outputs.Renditions {
		pl, text := r.playlist(rendition.Playlist)
		if !pl.Ended || len(pl.Segments) != 4 || pl.MediaSequence != 0 || !strings.Contains(text, "#EXT-X-PLAYLIST-TYPE:VOD") {
			t.Errorf("rung %s after the stream:\n%s", rendition.Name, text)
		}
		for _, file := range pl.Files() {
			if !r.exists(filepath.ToSlash(filepath.Join(filepath.Dir(rendition.Playlist), file))) {
				t.Errorf("rung %s lists %s, which is not in the store", rendition.Name, file)
			}
		}
		if r.exists(strings.TrimSuffix(rendition.Playlist, "index.m3u8") + "dvr.m3u8") {
			t.Errorf("rung %s still has a whole-stream playlist beside the VOD", rendition.Name)
		}
	}
	done := r.waitStream(v.ID, StateEnded)
	if done.EndedAt == nil || done.Error != "" {
		t.Errorf("the ended stream is %+v", done)
	}

	// The key is single-use. A stream that has ended is not restarted by
	// whoever still has its key.
	if _, err := r.m.Publish(context.Background(), App, v.StreamKey, remote); err == nil {
		t.Error("the key of an ended stream was accepted")
	}
	// The key appears nowhere it should not: not in the log, and not on
	// ffmpeg's command line, where `ps` would show it.
	secret := strings.SplitN(v.StreamKey, "_", 2)[1]
	if strings.Contains(r.logs.String(), secret) {
		t.Errorf("the stream key was logged:\n%s", r.logs.String())
	}
	if argv, _ := os.ReadFile(r.argvLog); strings.Contains(string(argv), secret) || !strings.Contains(string(argv), "pipe:0") {
		t.Errorf("ffmpeg's argv:\n%s", argv)
	}
	// The work directory is not left behind. It is removed as the session
	// returns, which is a moment after the stream is recorded as ended.
	deadline := time.Now().Add(10 * time.Second)
	for {
		left, _ := os.ReadDir(r.m.cfg.WorkDir)
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("%d entries left in the live work directory", len(left))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestABroadcasterWhoDropsAndComesBackContinuesTheSameStream(t *testing.T) {
	r := newRig(t, "ok", nil)
	v := r.create()

	first := r.publish(v.StreamKey)
	broadcast(t, first, 5000) // segments at 2s and 4s
	r.segments(v.AssetID, 2)
	first.Close(errors.New("connection reset by peer"))

	// Dropped, not ended: the stream is still live and the asset still growing.
	time.Sleep(100 * time.Millisecond)
	if s, _ := r.repo.Get(context.Background(), v.ID); s.State != StateLive {
		t.Fatalf("the stream is %q a moment after the broadcaster dropped\n%s", s.State, r.logs.String())
	}
	if a, _ := r.assets.Get(context.Background(), v.AssetID); a.State != assets.StateProcessing {
		t.Fatalf("the asset is %q a moment after the broadcaster dropped", a.State)
	}

	// Back, on a new connection whose clock starts again at zero. The encoder
	// must see one timeline: the fake refuses a timestamp that goes backwards,
	// and a stream whose encoder died would end here with two segments.
	second := r.publish(v.StreamKey)
	broadcast(t, second, 5000)
	r.segments(v.AssetID, 5)
	second.Close(nil)

	vod := r.waitAsset(v.AssetID, ready)
	if vod.DurationMs != 10000 {
		t.Errorf("the VOD is %dms; both sides of the reconnect are 10000", vod.DurationMs)
	}
	pl, text := r.playlist("out/" + v.AssetID + "/720p/index.m3u8")
	if len(pl.Segments) != 5 || !pl.Ended {
		t.Errorf("the VOD playlist:\n%s", text)
	}
	if strings.Contains(r.logs.String(), "non-monotonic") || strings.Contains(r.logs.String(), "the encoder stopped") {
		t.Errorf("the encoder did not survive the reconnect:\n%s", r.logs.String())
	}
	// One stream, one asset, one ffmpeg.
	argv, _ := os.ReadFile(r.argvLog)
	if got := strings.Count(string(argv), "pipe:0"); got != 1 {
		t.Errorf("%d encoders were started for one stream", got)
	}
	list, _ := r.assets.List(context.Background(), 0)
	if len(list) != 1 {
		t.Errorf("%d assets for one stream", len(list))
	}
}

func TestANewConnectionWithTheKeyReplacesTheOldOne(t *testing.T) {
	// A broadcaster whose network died reconnects before the server has
	// noticed the old connection is dead. The new one must not be turned away
	// to wait out a timeout: whoever holds the key and connected last is the
	// broadcaster.
	r := newRig(t, "ok", nil)
	v := r.create()
	old := r.publish(v.StreamKey)
	broadcast(t, old, 3000)
	r.segments(v.AssetID, 1)

	replacement := r.publish(v.StreamKey)
	if err := old.Tag(rtmp.TagVideo, 3100, []byte{0x17, 1, 0}); err == nil {
		t.Error("the replaced connection can still write into the stream")
	}
	// The old connection finally closing is not the broadcaster leaving.
	old.Close(errors.New("timeout"))
	broadcast(t, replacement, 3000)
	r.segments(v.AssetID, 3)
	if s, _ := r.repo.Get(context.Background(), v.ID); s.State != StateLive {
		t.Errorf("the stream is %q after the stale connection closed", s.State)
	}
	replacement.Close(nil)
	r.waitAsset(v.AssetID, ready)
}

func TestABroadcasterWhoDoesNotComeBackEndsTheStreamAfterTheGrace(t *testing.T) {
	r := newRig(t, "ok", func(c *Config) { c.Grace = 300 * time.Millisecond })
	v := r.create()
	sink := r.publish(v.StreamKey)
	broadcast(t, sink, 5000)
	r.segments(v.AssetID, 2)
	dropped := time.Now()
	sink.Close(errors.New("EOF"))

	vod := r.waitAsset(v.AssetID, ready)
	if waited := time.Since(dropped); waited < 250*time.Millisecond {
		t.Errorf("the stream ended %s after the drop; the grace is 300ms", waited)
	}
	// Everything the encoder had been given is in the VOD — closing its input
	// flushes it — and the stream says why it ended.
	if vod.DurationMs != 4000 {
		t.Errorf("duration %dms", vod.DurationMs)
	}
	r.waitStream(v.ID, StateEnded)
	if !strings.Contains(r.logs.String(), "did not come back") {
		t.Errorf("the reason was not logged:\n%s", r.logs.String())
	}
}

func TestAStreamCanBeEndedThroughTheAPI(t *testing.T) {
	r := newRig(t, "ok", nil)
	ctx := context.Background()

	// Live: the broadcast is cut off and what there was becomes the VOD.
	v := r.create()
	sink := r.publish(v.StreamKey)
	broadcast(t, sink, 5000)
	r.segments(v.AssetID, 2)
	if _, err := r.m.End(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	r.waitAsset(v.AssetID, ready)
	r.waitStream(v.ID, StateEnded)
	if err := sink.Tag(rtmp.TagVideo, 5100, []byte{0x17, 1, 0}); err == nil {
		t.Error("a broadcaster can still write into a stream that was ended")
	}

	// Idle: ending it revokes the key before it was ever used.
	unused := r.create()
	got, err := r.m.End(ctx, unused.ID)
	if err != nil || got.State != StateEnded || got.EndedAt == nil {
		t.Errorf("ending an idle stream: %+v, %v", got, err)
	}
	if _, err := r.m.Publish(ctx, App, unused.StreamKey, remote); err == nil {
		t.Error("a revoked key was accepted")
	}
	// Ending it twice is not an error and changes nothing.
	if again, err := r.m.End(ctx, unused.ID); err != nil || again.State != StateEnded {
		t.Errorf("ending an ended stream: %+v, %v", again, err)
	}
	if _, err := r.m.End(ctx, "no-such-stream"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ending a stream that does not exist: %v", err)
	}
}

func TestAnEncoderThatCrashesLeavesAShorterVOD(t *testing.T) {
	r := newRig(t, fakeffmpeg.CrashLive, nil)
	v := r.create()
	sink := r.publish(v.StreamKey)
	// The fake dies at the second segment with one rung a segment ahead.
	// Writes after that fail, as they would into a dead ffmpeg's pipe.
	_ = sink.Tag(rtmp.TagVideo, 0, []byte{0x17, 0, 0, 0, 0})
	for ts := 0; ts <= 9000; ts += 100 {
		if err := sink.Tag(rtmp.TagVideo, uint32(ts), []byte{0x17, 1, 0, 0, 0}); err != nil {
			break
		}
	}
	vod := r.waitAsset(v.AssetID, ended)
	if vod.State != assets.StateReady {
		t.Fatalf("two published segments were thrown away because the encoder died: %+v", vod)
	}
	if vod.DurationMs != 4000 {
		t.Errorf("duration %dms, want the 4000 every rung has", vod.DurationMs)
	}
	for _, rendition := range vod.Outputs.Renditions {
		pl, text := r.playlist(rendition.Playlist)
		if !pl.Ended || len(pl.Segments) != 2 {
			t.Errorf("rung %s was not cut back to what every rung has and closed:\n%s", rendition.Name, text)
		}
	}
	r.waitStream(v.ID, StateEnded)
}

func TestABroadcastThatCannotBeReadGivesTheKeyBack(t *testing.T) {
	r := newRig(t, fakeffmpeg.ProbeFail, nil)
	v := r.create()
	sink := r.publish(v.StreamKey)
	broadcast(t, sink, 1500)

	// Nothing was published, so nothing is spent: the stream is idle again
	// with the reason on it, and there is no asset.
	deadline := time.Now().Add(20 * time.Second)
	for {
		s, _ := r.repo.Get(context.Background(), v.ID)
		if s.State == StateIdle && s.Error != "" {
			if !strings.Contains(s.Error, "Invalid data") {
				t.Errorf("the reason is not ffprobe's: %q", s.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream is %+v", s)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := r.assets.Get(context.Background(), v.AssetID); !errors.Is(err, assets.ErrNotFound) {
		t.Errorf("an asset exists for a stream that never encoded a frame (%v)", err)
	}
	if err := sink.Tag(rtmp.TagVideo, 1600, []byte{0x17, 1, 0}); err == nil {
		t.Error("the refused connection can still write")
	}

	// The broadcaster fixes their encoder and tries again with the same key.
	t.Setenv(fakeffmpeg.EnvMode, "ok")
	// The first session must have let go before the key can be claimed again.
	deadline = time.Now().Add(10 * time.Second)
	var retry rtmp.Sink
	for {
		var err error
		if retry, err = r.m.Publish(context.Background(), App, v.StreamKey, remote); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the key was not usable again: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	broadcast(t, retry, 5000)
	r.waitAsset(v.AssetID, growing)
	retry.Close(nil)
	r.waitAsset(v.AssetID, ready)
}

func TestStoppingTheNodeClosesItsStreamsIntoVODs(t *testing.T) {
	// A deploy is a SIGTERM. The broadcast up to that moment is a recording
	// somebody made, and it must be a finished asset by the time the process
	// exits — not a playlist left open for ever.
	r := newRig(t, "ok", nil)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.m.Run(ctx) }()

	v := r.create()
	sink := r.publish(v.StreamKey)
	broadcast(t, sink, 7000)
	r.segments(v.AssetID, 3)

	stop()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the node was told to stop")
	}
	// By the time Run has returned — not eventually.
	a, err := r.assets.Get(context.Background(), v.AssetID)
	if err != nil || a.State != assets.StateReady || a.Growing || a.DurationMs != 6000 {
		t.Errorf("after the node stopped the asset is %+v (%v)", a, err)
	}
	if s, _ := r.repo.Get(context.Background(), v.ID); s.State != StateEnded {
		t.Errorf("after the node stopped the stream is %q", s.State)
	}
	// And a node that is stopping takes no new broadcasts.
	other := r.create()
	if _, err := r.m.Publish(context.Background(), App, other.StreamKey, remote); err == nil {
		t.Error("a stopping node accepted a new broadcast")
	}
}

func TestEveryRefusalLooksTheSameAndNoneLogsTheKey(t *testing.T) {
	r := newRig(t, "ok", nil)
	ctx := context.Background()
	v := r.create()
	endedStream := r.create()
	if _, err := r.m.End(ctx, endedStream.ID); err != nil {
		t.Fatal(err)
	}
	wrongSecret := v.ID + "_" + strings.Repeat("0", 48)

	cases := map[string]struct{ app, key string }{
		"the wrong application":            {"vod", v.StreamKey},
		"no application":                   {"", v.StreamKey},
		"a key with no secret":             {App, v.ID},
		"an empty key":                     {App, ""},
		"a stream that does not exist":     {App, "01NOSUCHSTREAM_" + strings.Repeat("a", 48)},
		"the right stream, the wrong key":  {App, wrongSecret},
		"the key of another stream":        {App, v.ID + "_" + strings.SplitN(endedStream.StreamKey, "_", 2)[1]},
		"the right key for an ended one":   {App, endedStream.StreamKey},
		"the key with something appended":  {App, v.StreamKey + "0"},
		"the key in a different case":      {App, strings.ToLower(v.StreamKey)},
		"SQL where the stream id would be": {App, "' OR 1=1 --_" + strings.Repeat("a", 48)},
	}
	var first error
	for name, c := range cases {
		sink, err := r.m.Publish(ctx, c.app, c.key, remote)
		if err == nil {
			sink.Close(nil)
			t.Errorf("%s: accepted", name)
			continue
		}
		// One error for all of them. What the RTMP server tells a client is
		// derived from this, and it must not distinguish "no such stream"
		// from "wrong key".
		if first == nil {
			first = err
		}
		if err != first {
			t.Errorf("%s: refused with %q, others with %q", name, err, first)
		}
	}
	// None of that moved the real stream, and its key still works.
	if s, _ := r.repo.Get(ctx, v.ID); s.State != StateIdle {
		t.Errorf("refused publishes left the stream %q", s.State)
	}
	logs := r.logs.String()
	for _, secret := range []string{strings.SplitN(v.StreamKey, "_", 2)[1], strings.SplitN(endedStream.StreamKey, "_", 2)[1], strings.Repeat("0", 48)} {
		if strings.Contains(logs, secret) {
			t.Errorf("a presented key was logged:\n%s", logs)
		}
	}
	// A wrong key for a real stream is worth an operator's attention, by id.
	if !strings.Contains(logs, "wrong key") || !strings.Contains(logs, v.ID) {
		t.Errorf("a wrong key for a real stream was not logged by stream id:\n%s", logs)
	}
	sink := r.publish(v.StreamKey)
	sink.Close(errors.New("gone"))
}

func TestOnlyOneNodeGetsAStream(t *testing.T) {
	// The same key presented to several nodes at once: the row is the lock.
	repo := OpenRepo(testdb.Pool(t))
	ctx := context.Background()
	if _, err := repo.Create(ctx, "S", HashKey("S_x"), "A"); err != nil {
		t.Fatal(err)
	}
	const nodes = 8
	var wg sync.WaitGroup
	wins := make(chan string, nodes)
	for i := 0; i < nodes; i++ {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			if _, err := repo.Claim(ctx, "S", node, time.Minute); err == nil {
				wins <- node
			} else if !errors.Is(err, ErrNotHeld) {
				t.Errorf("claim: %v", err)
			}
		}(string(rune('a' + i)))
	}
	wg.Wait()
	close(wins)
	var winners []string
	for w := range wins {
		winners = append(winners, w)
	}
	if len(winners) != 1 {
		t.Fatalf("%d nodes claimed one stream: %v", len(winners), winners)
	}
	holder := winners[0]

	// Only the holder can renew, and only the holder can finish.
	if err := repo.Heartbeat(ctx, "S", holder, time.Minute); err != nil {
		t.Errorf("the holder cannot renew: %v", err)
	}
	if err := repo.Heartbeat(ctx, "S", "someone-else", time.Minute); !errors.Is(err, ErrNotHeld) {
		t.Errorf("another node renewed: %v", err)
	}
	if err := repo.Finish(ctx, "S", "someone-else", ""); !errors.Is(err, ErrNotHeld) {
		t.Errorf("another node finished the stream: %v", err)
	}
	// Ended through the API: the holder's next heartbeat is how it finds out.
	if s, err := repo.RequestEnd(ctx, "S"); err != nil || s.State != StateEnding {
		t.Errorf("request end: %+v, %v", s, err)
	}
	if err := repo.Heartbeat(ctx, "S", holder, time.Minute); !errors.Is(err, ErrNotHeld) {
		t.Errorf("the holder's heartbeat after an end request: %v", err)
	}
	if err := repo.Finish(ctx, "S", holder, ""); err != nil {
		t.Errorf("the holder cannot finish a stream it was asked to end: %v", err)
	}
	if s, _ := repo.Get(ctx, "S"); s.State != StateEnded || s.EndedAt == nil {
		t.Errorf("finished stream: %+v", s)
	}
	// A stream with a live lease is not an orphan and cannot be adopted.
	if _, err := repo.Create(ctx, "T", HashKey("T_x"), "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Claim(ctx, "T", "alive", time.Minute); err != nil {
		t.Fatal(err)
	}
	if orphans, _ := repo.Orphans(ctx); len(orphans) != 0 {
		t.Errorf("a stream whose node is renewing it is listed as abandoned: %v", orphans)
	}
	if _, err := repo.Adopt(ctx, "T", "thief", time.Minute); !errors.Is(err, ErrNotHeld) {
		t.Errorf("a stream was taken from a live node: %v", err)
	}
}

// publishOrphan puts in the store what a node would have published before it
// died: per rung, a windowed live playlist, the whole-stream one, and the
// segments.
func publishOrphan(t *testing.T, r *rig, assetID string, segmentsPerRung map[string]int, window int) assets.Outputs {
	t.Helper()
	ctx := context.Background()
	out := assets.Outputs{}
	for _, rung := range []string{"720p", "360p"} {
		n := segmentsPerRung[rung]
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"init_0.mp4\"\n")
		prefix := "out/" + assetID + "/" + rung + "/"
		for i := 0; i < n; i++ {
			name := "seg_0000" + string(rune('0'+i)) + ".m4s"
			b.WriteString("#EXTINF:2.000000,\n" + name + "\n")
			if _, err := r.objects.Put(ctx, prefix+name, strings.NewReader("segment")); err != nil {
				t.Fatal(err)
			}
		}
		whole, err := pipeline.ParseMediaPlaylist([]byte(b.String()))
		if err != nil {
			t.Fatal(err)
		}
		if window > 0 {
			if _, err := r.objects.Put(ctx, prefix+publish.DVRPlaylist, strings.NewReader(string(whole.Render(0, false)))); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := r.objects.Put(ctx, prefix+publish.RungPlaylist, strings.NewReader(string(whole.Render(window, false)))); err != nil {
			t.Fatal(err)
		}
		out.Renditions = append(out.Renditions, assets.Rendition{Name: rung, Playlist: prefix + publish.RungPlaylist})
	}
	return out
}

func TestAStreamWhoseNodeDiedIsClosedByWhoeverNotices(t *testing.T) {
	for _, window := range []int{3, 0} {
		r := newRig(t, "ok", nil)
		ctx := context.Background()
		v := r.create()
		// Another node claimed it, started publishing, and stopped renewing.
		if _, err := r.repo.Claim(ctx, v.ID, "n-dead", time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if _, err := r.assets.CreateLive(ctx, v.AssetID); err != nil {
			t.Fatal(err)
		}
		// It died with one rung a segment ahead of the other.
		out := publishOrphan(t, r, v.AssetID, map[string]int{"720p": 6, "360p": 5}, window)
		if err := r.assets.MarkPublished(ctx, v.AssetID, v.Address, out); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond) // the lease lapses

		r.m.adoptOrphans(ctx)

		a, err := r.assets.Get(ctx, v.AssetID)
		if err != nil || a.State != assets.StateReady || a.Growing || a.Address != v.Address {
			t.Fatalf("window %d: the abandoned stream's asset is %+v (%v)", window, a, err)
		}
		if a.DurationMs != 10000 {
			t.Errorf("window %d: duration %dms, want the 10000 both rungs have", window, a.DurationMs)
		}
		for _, rendition := range a.Outputs.Renditions {
			pl, text := r.playlist(rendition.Playlist)
			// The whole stream from its first segment — not the window that
			// happened to be published last — cut to what every rung has.
			if !pl.Ended || len(pl.Segments) != 5 || pl.MediaSequence != 0 || pl.Segments[0].URI != "seg_00000.m4s" {
				t.Errorf("window %d: rung %s was closed as:\n%s", window, rendition.Name, text)
			}
			if r.exists(strings.TrimSuffix(rendition.Playlist, "index.m3u8") + "dvr.m3u8") {
				t.Errorf("window %d: rung %s still has its whole-stream playlist", window, rendition.Name)
			}
		}
		if s, _ := r.repo.Get(ctx, v.ID); s.State != StateEnded || s.Error != "" {
			t.Errorf("window %d: the abandoned stream is %+v", window, s)
		}
		// Nothing left to adopt, and doing it again is harmless.
		r.m.adoptOrphans(ctx)
		if orphans, _ := r.repo.Orphans(ctx); len(orphans) != 0 {
			t.Errorf("window %d: still listed as abandoned: %v", window, orphans)
		}
	}
}

func TestAStreamWhoseNodeDiedBeforePublishingIsFailedNotLeftOpen(t *testing.T) {
	r := newRig(t, "ok", nil)
	ctx := context.Background()

	// Died after creating the asset and before the first segment.
	v := r.create()
	if _, err := r.repo.Claim(ctx, v.ID, "n-dead", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := r.assets.CreateLive(ctx, v.AssetID); err != nil {
		t.Fatal(err)
	}
	// Died before it had even created the asset.
	bare := r.create()
	if _, err := r.repo.Claim(ctx, bare.ID, "n-dead", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	r.m.adoptOrphans(ctx)

	a, err := r.assets.Get(ctx, v.AssetID)
	if err != nil || a.State != assets.StateFailed || a.Error == "" {
		t.Errorf("an asset with nothing published is %+v (%v); it must not stay in processing for ever", a, err)
	}
	if s, _ := r.repo.Get(ctx, v.ID); s.State != StateEnded || s.Error == "" {
		t.Errorf("its stream is %+v", s)
	}
	if s, _ := r.repo.Get(ctx, bare.ID); s.State != StateEnded {
		t.Errorf("the stream with no asset is %+v", s)
	}
	if _, err := r.assets.Get(ctx, bare.AssetID); !errors.Is(err, assets.ErrNotFound) {
		t.Errorf("an asset was invented for a stream that never encoded (%v)", err)
	}
}

func TestClosingFromTheStoreLeavesFinishedAssetsAlone(t *testing.T) {
	r := newRig(t, "ok", nil)
	ctx := context.Background()
	v := r.create()
	if _, err := r.assets.CreateLive(ctx, v.AssetID); err != nil {
		t.Fatal(err)
	}
	out := publishOrphan(t, r, v.AssetID, map[string]int{"720p": 4, "360p": 4}, 0)
	out.DurationMs = 123
	if err := r.assets.MarkReady(ctx, v.AssetID, v.Address, out); err != nil {
		t.Fatal(err)
	}
	before, _ := r.playlist(out.Renditions[0].Playlist)
	if err := CloseFromStore(ctx, r.assets, r.objects, v.AssetID); err != nil {
		t.Fatal(err)
	}
	a, _ := r.assets.Get(ctx, v.AssetID)
	after, _ := r.playlist(out.Renditions[0].Playlist)
	if a.DurationMs != 123 || after.Ended != before.Ended {
		t.Errorf("a ready asset was rewritten: %+v", a)
	}
	// And an asset that does not exist is not an error.
	if err := CloseFromStore(ctx, r.assets, r.objects, "no-such-asset"); err != nil {
		t.Errorf("closing a stream with no asset: %v", err)
	}
}

// flaky is a store that can be told to refuse every write for a while.
type flaky struct {
	store.Store
	mu      sync.Mutex
	failing bool
	refused int
}

func (f *flaky) Put(ctx context.Context, key string, r io.Reader) (store.Object, error) {
	f.mu.Lock()
	failing := f.failing
	if failing {
		f.refused++
	}
	f.mu.Unlock()
	if failing {
		return store.Object{}, errors.New("the store is not answering")
	}
	return f.Store.Put(ctx, key, r)
}

func (f *flaky) set(failing bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = failing
	return f.refused
}

func TestAStoreThatStopsAnsweringForAMomentDoesNotEndTheBroadcast(t *testing.T) {
	var objects *flaky
	r := newRig(t, "ok", func(c *Config) {
		objects = &flaky{Store: c.Store}
		c.Store = objects
	})
	v := r.create()
	sink := r.publish(v.StreamKey)
	broadcast(t, sink, 3000)
	r.segments(v.AssetID, 1)

	// The store goes away, and the broadcast carries on into it.
	objects.set(true)
	for ts := 3100; ts <= 9000; ts += 100 {
		if err := sink.Tag(rtmp.TagVideo, uint32(ts), []byte{0x17, 1, 0, 0, 0}); err != nil {
			t.Fatalf("the broadcaster was cut off while the store was down: %v", err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for objects.set(true) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing was written to the store while it was down; the test proves nothing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s, _ := r.repo.Get(context.Background(), v.ID); s.State != StateLive {
		t.Fatalf("one refused write ended the stream (state %q)\n%s", s.State, r.logs.String())
	}

	// It comes back. Everything encoded in the meantime is published: nothing
	// was dropped, only delayed.
	objects.set(false)
	r.segments(v.AssetID, 4)
	sink.Close(nil)
	vod := r.waitAsset(v.AssetID, ready)
	if vod.DurationMs != 8000 {
		t.Errorf("the VOD is %dms; the segments encoded while the store was down are 8000 with the rest", vod.DurationMs)
	}
	pl, text := r.playlist("out/" + v.AssetID + "/720p/index.m3u8")
	if len(pl.Segments) != 4 || !pl.Ended {
		t.Errorf("the VOD playlist:\n%s", text)
	}
	if !strings.Contains(r.logs.String(), "publishing recovered") {
		t.Errorf("the recovery was not logged:\n%s", r.logs.String())
	}
}
