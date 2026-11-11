package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aura/media/internal/pipeline"
	"aura/media/internal/store"
)

// encoder stands in for ffmpeg: it writes the files ffmpeg's HLS muxer writes,
// in the order it writes them, so the publisher can be driven one step at a
// time with no ffmpeg on the machine.
type encoder struct {
	t     *testing.T
	dir   string
	rungs []string
	seg   int // segment duration, seconds
	// listed is how many segments each rung's playlist names.
	listed map[string]int
}

func newEncoder(t *testing.T, seg int, rungs ...string) *encoder {
	t.Helper()
	e := &encoder{t: t, dir: t.TempDir(), rungs: rungs, seg: seg, listed: map[string]int{}}
	for _, r := range rungs {
		if err := os.MkdirAll(filepath.Join(e.dir, r), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func segName(i int) string { return fmt.Sprintf("seg_%05d.m4s", i) }

func (e *encoder) write(rel, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// open starts a segment: the file exists and is being written, and the
// playlist does not name it. This is the state a segment is in for seconds at
// a time during a real encode.
func (e *encoder) open(rung string, i int) {
	e.write(rung+"/"+segName(i), "half of "+rung+" segment")
}

// close finishes a segment and lists it, the way ffmpeg does: file complete
// first, playlist rewritten second.
func (e *encoder) close(rung string, i int, ended bool) {
	e.write(rung+"/init_0.mp4", "init of "+rung)
	e.write(rung+"/"+segName(i), fmt.Sprintf("%s segment %d, complete", rung, i))
	e.listed[rung] = i + 1
	e.write(rung+"/"+RungPlaylist, e.playlist(rung, ended))
}

func (e *encoder) playlist(rung string, ended bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"init_0.mp4\"\n", e.seg)
	for i := 0; i < e.listed[rung]; i++ {
		fmt.Fprintf(&b, "#EXTINF:%d.000000,\n%s\n", e.seg, segName(i))
	}
	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

func (e *encoder) master() {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	for _, r := range e.rungs {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360,CODECS=\"avc1.4d401e,mp4a.40.2\"\n%s/index.m3u8\n\n", r)
	}
	e.write(pipeline.MasterPlaylist, b.String())
}

// all closes segment i on every rung.
func (e *encoder) all(i int, ended bool) {
	for _, r := range e.rungs {
		e.close(r, i, ended)
	}
}

// recorder is a store that remembers the order things were put in.
type recorder struct {
	store.Store
	mu   sync.Mutex
	puts []string
	// fail makes Put refuse keys with this suffix, once armed.
	fail string
}

func (r *recorder) Put(ctx context.Context, key string, body io.Reader) (store.Object, error) {
	r.mu.Lock()
	refuse := r.fail != "" && strings.HasSuffix(key, r.fail)
	r.mu.Unlock()
	if refuse {
		return store.Object{}, errors.New("the store is full")
	}
	obj, err := r.Store.Put(ctx, key, body)
	if err == nil {
		r.mu.Lock()
		r.puts = append(r.puts, key)
		r.mu.Unlock()
	}
	return obj, err
}

func (r *recorder) index(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.puts) - 1; i >= 0; i-- {
		if r.puts[i] == key {
			return i
		}
	}
	return -1
}

func newStore(t *testing.T) *recorder {
	t.Helper()
	fs, err := store.NewFS(t.TempDir(), "", "/media")
	if err != nil {
		t.Fatal(err)
	}
	return &recorder{Store: fs}
}

func has(t *testing.T, s store.Store, key string) bool {
	t.Helper()
	body, _, err := s.Open(context.Background(), key)
	if err != nil {
		return false
	}
	body.Close()
	return true
}

func read(t *testing.T, s store.Store, key string) string {
	t.Helper()
	body, _, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("%s is not in the store: %v", key, err)
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestASegmentIsUploadedOnlyOnceItIsListed(t *testing.T) {
	ctx := context.Background()
	enc := newEncoder(t, 4, "720p", "360p")
	s := newStore(t)
	pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}

	// Nothing written yet: nothing published, and no error for there being
	// nothing.
	status, err := pub.Sync(ctx)
	if err != nil || status.Playable || status.Segments != 0 {
		t.Fatalf("empty directory: %+v, %v", status, err)
	}

	// ffmpeg is writing the first segment of each rung. The files exist.
	enc.open("720p", 0)
	enc.open("360p", 0)
	if status, err = pub.Sync(ctx); err != nil || status.Playable {
		t.Fatalf("segments open but not listed: %+v, %v", status, err)
	}
	if has(t, s, "out/a/720p/seg_00000.m4s") {
		t.Fatal("a segment ffmpeg was still writing was uploaded")
	}

	// One rung closes its first segment; the other has not. The address must
	// not be handed out: the master playlist names both.
	enc.close("720p", 0, false)
	enc.master()
	enc.open("720p", 1)
	if status, err = pub.Sync(ctx); err != nil || status.Playable {
		t.Fatalf("one rung of two has a segment: %+v, %v", status, err)
	}
	if got := read(t, s, "out/a/720p/seg_00000.m4s"); got != "720p segment 0, complete" {
		t.Fatalf("the listed segment was uploaded as %q", got)
	}
	if has(t, s, "out/a/720p/seg_00001.m4s") {
		t.Fatal("the next segment, open and unlisted, was uploaded")
	}
	if has(t, s, "out/a/master.m3u8") {
		t.Fatal("the master playlist was published while a rung it names had nothing to play")
	}

	// The second rung catches up. Now it is playable.
	enc.close("360p", 0, false)
	if status, err = pub.Sync(ctx); err != nil || !status.Playable || status.Segments != 1 || status.DurationMs != 4000 {
		t.Fatalf("both rungs have a segment: %+v, %v", status, err)
	}
	for _, key := range []string{"out/a/master.m3u8", "out/a/720p/index.m3u8", "out/a/360p/index.m3u8", "out/a/720p/init_0.mp4", "out/a/360p/init_0.mp4", "out/a/360p/seg_00000.m4s"} {
		if !has(t, s, key) {
			t.Errorf("playable, and %s is not in the store", key)
		}
	}

	// The order is the invariant: each playlist after every file it names, and
	// the master after every rung's playlist.
	for _, rung := range enc.rungs {
		playlist := s.index("out/a/" + rung + "/index.m3u8")
		for _, file := range []string{"init_0.mp4", "seg_00000.m4s"} {
			if at := s.index("out/a/" + rung + "/" + file); at < 0 || at > playlist {
				t.Errorf("%s/%s was put at %d, after its playlist at %d", rung, file, at, playlist)
			}
		}
		if master := s.index("out/a/master.m3u8"); master < playlist {
			t.Errorf("the master playlist was put before the %s playlist", rung)
		}
	}

	// A pass with nothing new uploads nothing.
	before := len(s.puts)
	if _, err := pub.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.puts) != before {
		t.Errorf("an idle sync put %v", s.puts[before:])
	}
}

func TestThePublishedPlaylistNeverNamesAMissingSegment(t *testing.T) {
	// Walk a whole encode a step at a time. After every step, everything the
	// store's playlists name must be in the store.
	ctx := context.Background()
	enc := newEncoder(t, 4, "720p", "480p", "360p")
	s := newStore(t)
	pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
	const total = 6

	check := func(step string) {
		t.Helper()
		for _, rung := range enc.rungs {
			key := "out/a/" + rung + "/index.m3u8"
			if !has(t, s, key) {
				continue
			}
			pl, err := pipeline.ParseMediaPlaylist([]byte(read(t, s, key)))
			if err != nil || pl.Truncated {
				t.Fatalf("%s: the published %s playlist is damaged: %v", step, rung, err)
			}
			for _, name := range pl.Files() {
				if !has(t, s, "out/a/"+rung+"/"+name) {
					t.Fatalf("%s: the published %s playlist names %s, which is not in the store", step, rung, name)
				}
			}
		}
	}

	for i := 0; i < total; i++ {
		for _, rung := range enc.rungs {
			enc.open(rung, i)
			if _, err := pub.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			check(fmt.Sprintf("segment %d of %s open", i, rung))
			enc.close(rung, i, false)
			if i == 0 {
				enc.master()
			}
			if _, err := pub.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			check(fmt.Sprintf("segment %d of %s closed", i, rung))
		}
	}
	// ffmpeg exits: every playlist gets ENDLIST.
	for _, rung := range enc.rungs {
		enc.write(rung+"/"+RungPlaylist, enc.playlist(rung, true))
	}
	result, err := pub.Finish(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	check("finished")
	if result.Segments != total || result.DurationMs != total*4000 {
		t.Errorf("result %+v, want %d segments and %dms", result, total, total*4000)
	}
	for _, rung := range enc.rungs {
		// The VOD path publishes what ffmpeg wrote, not a rewrite of it.
		if got := read(t, s, "out/a/"+rung+"/index.m3u8"); got != enc.playlist(rung, true) {
			t.Errorf("the published %s playlist is not ffmpeg's:\n%s", rung, got)
		}
		if !result.Playlists[rung].Ended {
			t.Errorf("the %s result is not ended", rung)
		}
	}
}

func TestFinishRefusesAPackageThatIsNotWhole(t *testing.T) {
	ctx := context.Background()

	t.Run("no ENDLIST after a clean exit", func(t *testing.T) {
		enc := newEncoder(t, 4, "360p")
		enc.all(0, false)
		enc.master()
		pub := &Publisher{Store: newStore(t), Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
		if _, err := pub.Finish(ctx, true); err == nil || !strings.Contains(err.Error(), "ENDLIST") {
			t.Errorf("an unclosed playlist was accepted as finished: %v", err)
		}
	})

	t.Run("nothing encoded", func(t *testing.T) {
		enc := newEncoder(t, 4, "360p")
		pub := &Publisher{Store: newStore(t), Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
		if _, err := pub.Finish(ctx, true); err == nil {
			t.Error("an empty directory was accepted as a finished package")
		}
	})

	t.Run("no master playlist", func(t *testing.T) {
		enc := newEncoder(t, 4, "360p")
		enc.all(0, true)
		pub := &Publisher{Store: newStore(t), Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
		if _, err := pub.Finish(ctx, true); err == nil || !strings.Contains(err.Error(), pipeline.MasterPlaylist) {
			t.Errorf("a package with no master playlist was accepted: %v", err)
		}
	})

	t.Run("a segment that vanished from the store is put back", func(t *testing.T) {
		enc := newEncoder(t, 4, "360p")
		s := newStore(t)
		pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
		enc.all(0, false)
		enc.all(1, true)
		enc.master()
		if _, err := pub.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "out/a/360p/seg_00000.m4s"); err != nil {
			t.Fatal(err)
		}
		if _, err := pub.Finish(ctx, true); err != nil {
			t.Fatalf("the final check did not repair a missing segment: %v", err)
		}
		if got := read(t, s, "out/a/360p/seg_00000.m4s"); got != "360p segment 0, complete" {
			t.Errorf("repaired as %q", got)
		}
	})

	t.Run("a segment that vanished and cannot be put back fails the package", func(t *testing.T) {
		enc := newEncoder(t, 2, "360p")
		s := newStore(t)
		// DropLocal: the local copy is gone once uploaded, as in live.
		pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs, DropLocal: true}
		enc.all(0, false)
		enc.all(1, true)
		enc.master()
		if _, err := pub.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(enc.dir, "360p", segName(0))); !os.IsNotExist(err) {
			t.Fatalf("DropLocal left the uploaded segment on disk: %v", err)
		}
		if _, err := os.Stat(filepath.Join(enc.dir, "360p", "init_0.mp4")); err != nil {
			t.Fatalf("DropLocal removed the init segment, which ffmpeg is not done with: %v", err)
		}
		if err := s.Delete(ctx, "out/a/360p/seg_00001.m4s"); err != nil {
			t.Fatal(err)
		}
		if _, err := pub.Finish(ctx, true); err == nil {
			t.Error("a package with a hole in it was accepted")
		}
	})

	t.Run("a store that refuses a segment stops the sync", func(t *testing.T) {
		enc := newEncoder(t, 4, "360p")
		s := newStore(t)
		s.fail = "seg_00001.m4s"
		pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
		enc.all(0, false)
		enc.master()
		if _, err := pub.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		before := read(t, s, "out/a/360p/index.m3u8")
		enc.all(1, false)
		if _, err := pub.Sync(ctx); err == nil {
			t.Fatal("a refused upload was not reported")
		}
		// The playlist naming the segment that could not be stored was not
		// published over the one that did not name it.
		if got := read(t, s, "out/a/360p/index.m3u8"); got != before {
			t.Errorf("the playlist moved on past a segment that is not in the store:\n%s", got)
		}
	})
}

func TestAPlaylistCaughtMidWriteIsNotPublished(t *testing.T) {
	ctx := context.Background()
	enc := newEncoder(t, 4, "360p")
	s := newStore(t)
	pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
	enc.all(0, false)
	enc.master()
	if _, err := pub.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	good := read(t, s, "out/a/360p/index.m3u8")

	// The second segment is closed, and the playlist is read half-written:
	// the new entry's name is cut short.
	enc.close("360p", 1, false)
	whole := enc.playlist("360p", false)
	enc.write("360p/"+RungPlaylist, whole[:len(whole)-6])
	status, err := pub.Sync(ctx)
	if err != nil {
		t.Fatalf("a half-written playlist was treated as an error: %v", err)
	}
	if status.Segments != 1 {
		t.Errorf("%d segments counted from a playlist cut mid-entry", status.Segments)
	}
	if got := read(t, s, "out/a/360p/index.m3u8"); got != good {
		t.Errorf("a half-written playlist was published:\n%s", got)
	}
	// The next pass sees it whole.
	enc.write("360p/"+RungPlaylist, whole)
	if status, err = pub.Sync(ctx); err != nil || status.Segments != 2 {
		t.Fatalf("after the write completed: %+v, %v", status, err)
	}
}

func TestAPlaylistCannotLeadOutOfItsRung(t *testing.T) {
	ctx := context.Background()
	enc := newEncoder(t, 4, "360p")
	s := newStore(t)
	secret := filepath.Join(filepath.Dir(enc.dir), "secret")
	if err := os.WriteFile(secret, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secret)
	enc.write("360p/"+RungPlaylist, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\n../../secret\n")
	pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
	if _, err := pub.Sync(ctx); err == nil {
		t.Error("a playlist entry with a path in it was followed")
	}
	if len(s.puts) != 0 {
		t.Errorf("something was uploaded: %v", s.puts)
	}
}

func TestLiveWindowSlidesAndTheWholeStreamIsKept(t *testing.T) {
	ctx := context.Background()
	enc := newEncoder(t, 2, "720p", "360p")
	s := newStore(t)
	pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/live", Rungs: enc.rungs, Window: 3, DropLocal: true}

	lastSeq := int64(-1)
	for i := 0; i < 8; i++ {
		enc.all(i, false)
		if i == 0 {
			enc.master()
		}
		status, err := pub.Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Playable {
			t.Fatalf("not playable after segment %d", i)
		}
		for _, rung := range enc.rungs {
			window, err := pipeline.ParseMediaPlaylist([]byte(read(t, s, "out/live/"+rung+"/index.m3u8")))
			if err != nil {
				t.Fatal(err)
			}
			if len(window.Segments) > 3 {
				t.Fatalf("after segment %d the live playlist lists %d entries; the window is 3", i, len(window.Segments))
			}
			if window.Ended {
				t.Fatalf("a live window was closed")
			}
			if last := window.Segments[len(window.Segments)-1].URI; last != segName(i) {
				t.Fatalf("after segment %d the live edge is %s", i, last)
			}
			// The sequence number names the first entry.
			if window.Segments[0].URI != segName(int(window.MediaSequence)) {
				t.Fatalf("sequence %d but the first entry is %s", window.MediaSequence, window.Segments[0].URI)
			}
			if rung == "720p" {
				if window.MediaSequence < lastSeq {
					t.Fatalf("the media sequence went backwards: %d after %d", window.MediaSequence, lastSeq)
				}
				lastSeq = window.MediaSequence
			}
			// The whole stream, beside it.
			dvr, err := pipeline.ParseMediaPlaylist([]byte(read(t, s, "out/live/"+rung+"/dvr.m3u8")))
			if err != nil || len(dvr.Segments) != i+1 || dvr.MediaSequence != 0 {
				t.Fatalf("after segment %d the whole-stream playlist has %d entries from sequence %d (%v)", i, len(dvr.Segments), dvr.MediaSequence, err)
			}
		}
		// Every segment ever published is still in the store, window or not.
		for j := 0; j <= i; j++ {
			if !has(t, s, "out/live/720p/"+segName(j)) {
				t.Fatalf("segment %d left the store when it left the window", j)
			}
		}
	}
	if lastSeq != 5 {
		t.Errorf("after 8 segments in a window of 3 the sequence is %d, want 5", lastSeq)
	}
	// A master for the whole-stream playlists, naming them.
	if got := read(t, s, "out/live/dvr.m3u8"); !strings.Contains(got, "720p/dvr.m3u8") || strings.Contains(got, "index.m3u8") {
		t.Errorf("the whole-stream master is:\n%s", got)
	}
	// Local disk does not grow with the stream.
	left, _ := filepath.Glob(filepath.Join(enc.dir, "720p", "seg_*.m4s"))
	if len(left) != 0 {
		t.Errorf("%d uploaded segments were left on local disk", len(left))
	}

	// The stream ends cleanly. The index becomes the whole stream, closed.
	for _, rung := range enc.rungs {
		enc.write(rung+"/"+RungPlaylist, enc.playlist(rung, true))
	}
	result, err := pub.Finish(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Segments != 8 || result.DurationMs != 16000 {
		t.Errorf("result %+v", result)
	}
	for _, rung := range enc.rungs {
		text := read(t, s, "out/live/"+rung+"/index.m3u8")
		vod, err := pipeline.ParseMediaPlaylist([]byte(text))
		if err != nil || !vod.Ended || len(vod.Segments) != 8 || vod.MediaSequence != 0 {
			t.Errorf("the closed %s playlist: ended=%v, %d segments from %d (%v)", rung, vod.Ended, len(vod.Segments), vod.MediaSequence, err)
		}
		if !strings.Contains(text, "#EXT-X-PLAYLIST-TYPE:VOD") {
			t.Errorf("the closed %s playlist is not marked VOD", rung)
		}
		if has(t, s, "out/live/"+rung+"/dvr.m3u8") {
			t.Errorf("the whole-stream %s playlist outlived the stream", rung)
		}
	}
	if has(t, s, "out/live/dvr.m3u8") {
		t.Error("the whole-stream master outlived the stream")
	}
}

func TestAnEncoderThatDiedLeavesAShorterVideoNotABrokenOne(t *testing.T) {
	// ffmpeg is killed mid-stream. One rung got its fourth segment listed and
	// the other did not, and nothing has ENDLIST. What was published is still
	// a video: three segments of it, on both rungs.
	ctx := context.Background()
	for _, window := range []int{0, 3} {
		enc := newEncoder(t, 2, "720p", "360p")
		s := newStore(t)
		pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/live", Rungs: enc.rungs, Window: window, DropLocal: true}
		for i := 0; i < 3; i++ {
			enc.all(i, false)
		}
		enc.master()
		enc.close("720p", 3, false)
		enc.open("360p", 3)
		if _, err := pub.Sync(ctx); err != nil {
			t.Fatal(err)
		}

		if _, err := pub.Finish(ctx, true); window == 0 && err == nil {
			t.Error("a clean finish was claimed for playlists with no ENDLIST")
		}
		result, err := pub.Finish(ctx, false)
		if err != nil {
			t.Fatalf("window %d: %v", window, err)
		}
		if result.Segments != 3 || result.DurationMs != 6000 {
			t.Errorf("window %d: result %+v, want the 3 segments both rungs have", window, result)
		}
		for _, rung := range enc.rungs {
			text := read(t, s, "out/live/"+rung+"/index.m3u8")
			pl, err := pipeline.ParseMediaPlaylist([]byte(text))
			if err != nil || !pl.Ended || len(pl.Segments) != 3 {
				t.Errorf("window %d: the closed %s playlist has %d segments, ended=%v (%v)", window, rung, len(pl.Segments), pl.Ended, err)
			}
			if strings.Contains(text, segName(3)) {
				t.Errorf("window %d: %s lists a segment the other rung does not have", window, rung)
			}
		}
		if err := pipeline.CheckAligned(result.Playlists); err != nil {
			t.Errorf("window %d: %v", window, err)
		}
	}
}

func TestASecondAttemptStartsFromNothing(t *testing.T) {
	// Attempt one publishes part of a package and dies. Attempt two clears the
	// prefix and publishes a different, shorter package. Nothing of attempt
	// one may be left: not its extra segment, not its extra rung.
	ctx := context.Background()
	s := newStore(t)

	first := newEncoder(t, 4, "720p", "480p", "360p")
	for i := 0; i < 3; i++ {
		first.all(i, false)
	}
	first.master()
	if _, err := (&Publisher{Store: s, Dir: first.dir, Prefix: "out/a", Rungs: first.rungs}).Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !has(t, s, "out/a/480p/seg_00002.m4s") {
		t.Fatal("the first attempt did not publish")
	}

	deleter, ok := s.Store.(store.TreeDeleter)
	if !ok {
		t.Fatal("the filesystem store cannot delete a tree")
	}
	if err := deleter.DeleteTree(ctx, "out/a"); err != nil {
		t.Fatal(err)
	}

	second := newEncoder(t, 4, "720p", "360p")
	second.all(0, false)
	second.all(1, true)
	second.master()
	if _, err := (&Publisher{Store: s, Dir: second.dir, Prefix: "out/a", Rungs: second.rungs}).Finish(ctx, true); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"out/a/480p/index.m3u8", "out/a/480p/seg_00000.m4s", "out/a/720p/seg_00002.m4s"} {
		if has(t, s, gone) {
			t.Errorf("%s survived from the failed attempt", gone)
		}
	}
	if got := read(t, s, "out/a/master.m3u8"); strings.Contains(got, "480p") {
		t.Errorf("the master playlist still names the first attempt's ladder:\n%s", got)
	}
	if !has(t, s, "out/a/720p/seg_00001.m4s") {
		t.Error("the second attempt's package is not there")
	}
}

func TestFramesAreUploadedInTheOrderTheyHappened(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	dir := t.TempDir()
	for _, name := range []string{"frame_00010.jpg", "frame_00002.jpg", "frame_00001.jpg", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := Frames(ctx, s, "out/a", dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"out/a/frames/frame_00001.jpg", "out/a/frames/frame_00002.jpg", "out/a/frames/frame_00010.jpg"}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Errorf("keys %v, want %v", keys, want)
	}
	// No directory is no frames, not a failure.
	if keys, err := Frames(ctx, s, "out/a", filepath.Join(dir, "absent")); err != nil || len(keys) != 0 {
		t.Errorf("a missing frames directory gave %v, %v", keys, err)
	}
}

func TestAPlaylistThatCannotBeReadYetIsLookedAtAgainNotGivenUpOn(t *testing.T) {
	// On Windows a file cannot be opened during the instant it is being
	// renamed into place, and ffmpeg replaces each playlist every few
	// seconds. While ffmpeg runs that is "not this pass". Once it has exited
	// nobody else is writing, and the same failure is a real one.
	ctx := context.Background()
	enc := newEncoder(t, 2, "720p", "360p")
	s := newStore(t)
	pub := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
	enc.all(0, false)
	enc.master()
	if status, err := pub.Sync(ctx); err != nil || !status.Playable {
		t.Fatalf("%+v, %v", status, err)
	}
	published := read(t, s, "out/a/360p/index.m3u8")

	// The one portable way to make a read fail: the name is there and is not
	// a file that can be read.
	path := filepath.Join(enc.dir, "360p", RungPlaylist)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	enc.close("720p", 1, false)
	status, err := pub.Sync(ctx)
	if err != nil {
		t.Fatalf("a playlist that could not be read for one pass ended the publish: %v", err)
	}
	// The rung that could not be read stays where it was; the other moves on.
	if status.Segments != 1 || !status.Playable {
		t.Errorf("status %+v", status)
	}
	if got := read(t, s, "out/a/360p/index.m3u8"); got != published {
		t.Errorf("the unreadable rung's published playlist changed:\n%s", got)
	}
	if !has(t, s, "out/a/720p/seg_00001.m4s") {
		t.Error("the readable rung was held back by the unreadable one")
	}

	// ffmpeg has exited and it still cannot be read: that is a failure.
	if _, err := pub.Finish(ctx, false); err == nil {
		t.Error("a playlist that cannot be read after the encode ended was ignored")
	}

	// And when it could be read again in time, nothing was lost.
	other := &Publisher{Store: s, Dir: enc.dir, Prefix: "out/a", Rungs: enc.rungs}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	enc.close("360p", 1, true)
	enc.write("720p/"+RungPlaylist, enc.playlist("720p", true))
	if result, err := other.Finish(ctx, true); err != nil || result.Segments != 2 {
		t.Errorf("after the playlist became readable: %+v, %v", result, err)
	}
}
