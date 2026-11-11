package pipeline

import (
	"errors"
	"strings"
	"testing"
)

// An EVENT playlist as ffmpeg writes it part-way through an encode.
const growing = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MAP:URI="init_0.mp4"
#EXTINF:4.000000,
seg_00000.m4s
#EXTINF:4.000000,
seg_00001.m4s
#EXTINF:3.960000,
seg_00002.m4s
`

func TestOnlyListedSegmentsAreComplete(t *testing.T) {
	p, err := ParseMediaPlaylist([]byte(growing))
	if err != nil {
		t.Fatal(err)
	}
	if p.Ended || p.Truncated {
		t.Errorf("a growing playlist read as ended=%v truncated=%v", p.Ended, p.Truncated)
	}
	if p.Init != "init_0.mp4" {
		t.Errorf("init segment is %q", p.Init)
	}
	// Init first: a player needs it before any media segment.
	want := []string{"init_0.mp4", "seg_00000.m4s", "seg_00001.m4s", "seg_00002.m4s"}
	if got := p.Files(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("files are %v, want %v", got, want)
	}
	if p.DurationMs() != 11960 {
		t.Errorf("duration %dms, want 11960", p.DurationMs())
	}
	if p.TargetDuration != 4 || p.Version != 7 || !p.Independent {
		t.Errorf("header read as %+v", p)
	}

	ended, err := ParseMediaPlaylist([]byte(growing + "#EXT-X-ENDLIST\n"))
	if err != nil || !ended.Ended || len(ended.Segments) != 3 {
		t.Errorf("the closed playlist read as %+v, %v", ended, err)
	}
}

func TestAPlaylistCaughtMidWriteYieldsNothingMadeUp(t *testing.T) {
	// Every prefix of a playlist is something a reader could see if the write
	// were not atomic. Whatever the cut, the segments reported must be a
	// prefix of the real ones, each with its whole name — never a name cut
	// short, and never a segment whose EXTINF had not been followed by a URI.
	full := growing + "#EXT-X-ENDLIST\n"
	real := []string{"seg_00000.m4s", "seg_00001.m4s", "seg_00002.m4s"}
	for cut := 0; cut <= len(full); cut++ {
		p, err := ParseMediaPlaylist([]byte(full[:cut]))
		if err != nil {
			if !errors.Is(err, ErrNotAPlaylist) {
				t.Fatalf("cut at %d: %v", cut, err)
			}
			continue
		}
		if len(p.Segments) > len(real) {
			t.Fatalf("cut at %d: %d segments from a playlist of %d", cut, len(p.Segments), len(real))
		}
		for i, s := range p.Segments {
			if s.URI != real[i] {
				t.Fatalf("cut at %d: segment %d is %q, want %q", cut, i, s.URI, real[i])
			}
		}
		// A cut in the middle of a line is always recognised as one.
		if cut > 0 && full[cut-1] != '\n' && !p.Truncated {
			t.Errorf("cut at %d is mid-line and was not reported as truncated", cut)
		}
		// ENDLIST is only believed from a playlist that is all there.
		if p.Ended && cut != len(full) {
			t.Errorf("cut at %d: reported ended", cut)
		}
	}

	// The specific shapes, by name.
	cases := map[string]struct {
		text     string
		segments int
	}{
		"cut inside a segment name":  {growing[:len(growing)-5], 2},
		"cut after an EXTINF line":   {strings.TrimSuffix(growing, "seg_00002.m4s\n"), 2},
		"cut inside an EXTINF value": {strings.TrimSuffix(growing, "960000,\nseg_00002.m4s\n"), 2},
		"name with no newline yet":   {strings.TrimSuffix(growing, "\n"), 2},
	}
	for name, c := range cases {
		p, err := ParseMediaPlaylist([]byte(c.text))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(p.Segments) != c.segments || !p.Truncated {
			t.Errorf("%s: %d segments, truncated=%v; want %d and truncated", name, len(p.Segments), p.Truncated, c.segments)
		}
	}
}

func TestParseRefusesWhatIsNotAPlaylist(t *testing.T) {
	for _, raw := range []string{"", "<html>404</html>", "seg_00000.m4s\n", "\x00\x01\x02"} {
		if _, err := ParseMediaPlaylist([]byte(raw)); !errors.Is(err, ErrNotAPlaylist) {
			t.Errorf("%q: err = %v, want ErrNotAPlaylist", raw, err)
		}
	}
	if _, err := ParseMediaPlaylist([]byte("#EXTM3U\n#EXTINF:soon,\nseg_00000.m4s\n")); err == nil {
		t.Error("an EXTINF that is not a number was accepted")
	}
	// CRLF line endings are a playlist too.
	p, err := ParseMediaPlaylist([]byte(strings.ReplaceAll(growing, "\n", "\r\n")))
	if err != nil || len(p.Segments) != 3 || p.Segments[2].URI != "seg_00002.m4s" {
		t.Errorf("CRLF playlist read as %+v, %v", p.Segments, err)
	}
}

func many(n int) MediaPlaylist {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"init_0.mp4\"\n")
	for i := 0; i < n; i++ {
		b.WriteString("#EXTINF:2.000000,\nseg_" + pad5(i) + ".m4s\n")
	}
	p, err := ParseMediaPlaylist([]byte(b.String()))
	if err != nil {
		panic(err)
	}
	return p
}

func pad5(n int) string {
	s := itoa(n)
	for len(s) < 5 {
		s = "0" + s
	}
	return s
}

func TestWindowSlidesByExactlyWhatItDrops(t *testing.T) {
	// A player matches the playlist it just fetched against the last one by
	// media sequence number. If the number does not move by exactly the
	// entries dropped, it fetches a segment twice or skips one.
	for n := 1; n <= 12; n++ {
		text := string(many(n).Render(4, false))
		p, err := ParseMediaPlaylist([]byte(text))
		if err != nil {
			t.Fatalf("%d segments: rendered playlist does not parse: %v", n, err)
		}
		wantLen, wantSeq := n, 0
		if n > 4 {
			wantLen, wantSeq = 4, n-4
		}
		if len(p.Segments) != wantLen || p.MediaSequence != int64(wantSeq) {
			t.Fatalf("%d segments: window has %d starting at sequence %d, want %d at %d", n, len(p.Segments), p.MediaSequence, wantLen, wantSeq)
		}
		// Segment k of the stream is always sequence k: the first one listed
		// must be the one the sequence number names.
		if p.Segments[0].URI != "seg_"+pad5(wantSeq)+".m4s" {
			t.Fatalf("%d segments: sequence %d but first entry is %s", n, wantSeq, p.Segments[0].URI)
		}
		if p.Segments[len(p.Segments)-1].URI != "seg_"+pad5(n-1)+".m4s" {
			t.Fatalf("%d segments: the newest segment is not the last entry", n)
		}
		// A window is live: no type, and never an end.
		if strings.Contains(text, "PLAYLIST-TYPE") || strings.Contains(text, "ENDLIST") {
			t.Fatalf("%d segments: a live window carries a type or an end:\n%s", n, text)
		}
		if p.Init != "init_0.mp4" || !p.Independent || p.TargetDuration != 2 {
			t.Fatalf("%d segments: header lost in rendering: %+v", n, p)
		}
	}
}

func TestRenderClosesAStreamIntoAVOD(t *testing.T) {
	text := string(many(7).Render(0, true))
	if !strings.Contains(text, "#EXT-X-PLAYLIST-TYPE:VOD\n") {
		t.Errorf("a whole, closed playlist is not marked VOD:\n%s", text)
	}
	if !strings.HasSuffix(text, "#EXT-X-ENDLIST\n") {
		t.Errorf("ENDLIST is not the last line:\n%s", text)
	}
	p, err := ParseMediaPlaylist([]byte(text))
	if err != nil || !p.Ended || len(p.Segments) != 7 || p.MediaSequence != 0 {
		t.Errorf("closed playlist read back as %+v, %v", p, err)
	}
	// Whole and still open is an EVENT: entries are only ever appended.
	open := string(many(7).Render(0, false))
	if !strings.Contains(open, "#EXT-X-PLAYLIST-TYPE:EVENT\n") || strings.Contains(open, "ENDLIST") {
		t.Errorf("an open whole playlist rendered as:\n%s", open)
	}
	// Durations are written back exactly as ffmpeg wrote them.
	in, _ := ParseMediaPlaylist([]byte(growing))
	if out := string(in.Render(0, true)); !strings.Contains(out, "#EXTINF:3.960000,\nseg_00002.m4s\n") {
		t.Errorf("a duration changed in rendering:\n%s", out)
	}
}

func TestRenderCarriesTagsItDoesNotUnderstand(t *testing.T) {
	const text = "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-MAP:URI=\"init.mp4\"\n" +
		"#EXTINF:2.000000,\nseg_00000.m4s\n#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z\n#EXTINF:2.000000,\nseg_00001.m4s\n"
	p, err := ParseMediaPlaylist([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	out := string(p.Render(0, false))
	if !strings.Contains(out, "seg_00000.m4s\n#EXT-X-DISCONTINUITY\n#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z\n#EXTINF:2.000000,\nseg_00001.m4s\n") {
		t.Errorf("tags in front of a segment did not stay in front of it:\n%s", out)
	}
	// Dropped with the segment they belong to, not left dangling on the next.
	windowed := string(p.Render(1, false))
	if !strings.Contains(windowed, "#EXT-X-DISCONTINUITY\n") {
		t.Errorf("the surviving segment lost its tags:\n%s", windowed)
	}
	if strings.Contains(windowed, "seg_00000") {
		t.Errorf("the dropped segment is still listed:\n%s", windowed)
	}
}

func TestTruncateCutsBackToWhatEveryRungHas(t *testing.T) {
	p := many(5).Truncate(3)
	if len(p.Segments) != 3 || p.DurationMs() != 6000 {
		t.Errorf("truncated to %d segments, %dms", len(p.Segments), p.DurationMs())
	}
	if got := many(2).Truncate(9); len(got.Segments) != 2 {
		t.Errorf("truncating past the end changed the length to %d", len(got.Segments))
	}
}

func TestCheckAlignedCatchesALadderThatWouldStutter(t *testing.T) {
	if err := CheckAligned(map[string]MediaPlaylist{"720p": many(6), "480p": many(6), "360p": many(6)}); err != nil {
		t.Errorf("an aligned ladder was refused: %v", err)
	}
	if err := CheckAligned(map[string]MediaPlaylist{"720p": many(6)}); err != nil {
		t.Errorf("a one-rung ladder cannot be misaligned: %v", err)
	}
	if err := CheckAligned(map[string]MediaPlaylist{"720p": many(6), "360p": many(5)}); err == nil {
		t.Error("rungs with different segment counts were accepted")
	}
	// Same count, one boundary a frame late: the case an encoder that slips a
	// keyframe produces.
	late := many(6)
	late.Segments[2].Duration += 0.040
	late.Segments[3].Duration -= 0.040
	err := CheckAligned(map[string]MediaPlaylist{"720p": many(6), "360p": late})
	if err == nil {
		t.Fatal("a rung whose third segment is a frame longer was accepted")
	}
	if !strings.Contains(err.Error(), "segment 2") {
		t.Errorf("the refusal does not say where: %v", err)
	}
	// EXTINF rounding is not misalignment.
	rounded := many(6)
	rounded.Segments[1].Duration += 0.000001
	if err := CheckAligned(map[string]MediaPlaylist{"720p": many(6), "360p": rounded}); err != nil {
		t.Errorf("a microsecond of rounding was called misalignment: %v", err)
	}
}

func TestCheckMasterWantsEveryRungAndItsCodec(t *testing.T) {
	ladder := Ladder(Media{Width: 1280, Height: 720})
	const master = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-STREAM-INF:BANDWIDTH=3045188,AVERAGE-BANDWIDTH=2934026,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2"
720p/index.m3u8

#EXT-X-STREAM-INF:BANDWIDTH=1547058,RESOLUTION=854x480,CODECS="avc1.4d401e,mp4a.40.2"
480p/index.m3u8

#EXT-X-STREAM-INF:BANDWIDTH=927368,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2"
360p/index.m3u8

`
	if err := CheckMaster([]byte(master), ladder); err != nil {
		t.Errorf("ffmpeg's own master playlist was refused: %v", err)
	}
	// What an ffmpeg that cannot spell a codec writes: the variant, no CODECS.
	noCodecs := strings.Replace(master, `,CODECS="avc1.4d401e,mp4a.40.2"`, "", 1)
	if err := CheckMaster([]byte(noCodecs), ladder); err == nil || !strings.Contains(err.Error(), "CODECS") {
		t.Errorf("a variant with no CODECS was accepted: %v", err)
	}
	if err := CheckMaster([]byte(master), ladder[:2]); err == nil {
		t.Error("three variants for a two-rung ladder were accepted")
	}
	missing := strings.Replace(master, "360p/index.m3u8", "240p/index.m3u8", 1)
	if err := CheckMaster([]byte(missing), ladder); err == nil {
		t.Error("a master playlist that does not list a rung was accepted")
	}
	if err := CheckMaster([]byte("not a playlist"), ladder); !errors.Is(err, ErrNotAPlaylist) {
		t.Errorf("err = %v, want ErrNotAPlaylist", err)
	}
}
