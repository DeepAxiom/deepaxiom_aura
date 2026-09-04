package pipeline

import (
	"strings"
	"testing"
)

const probeJSON = `{
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080,
     "avg_frame_rate": "30000/1001", "r_frame_rate": "30000/1001"},
    {"codec_type": "audio", "codec_name": "aac"}
  ],
  "format": {"duration": "612.480000", "bit_rate": "4128000", "format_name": "mov,mp4,m4a"}
}`

func TestParseProbeReadsWhatTheLadderNeeds(t *testing.T) {
	m, err := ParseProbe([]byte(probeJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Width != 1920 || m.Height != 1080 {
		t.Errorf("dimensions %dx%d, want 1920x1080", m.Width, m.Height)
	}
	if m.DurationMs != 612480 {
		t.Errorf("duration %dms, want 612480", m.DurationMs)
	}
	if m.BitrateKbps != 4128 {
		t.Errorf("bitrate %dkbps, want 4128", m.BitrateKbps)
	}
	if !m.HasVideo || !m.HasAudio {
		t.Errorf("streams: video=%v audio=%v, want both", m.HasVideo, m.HasAudio)
	}
	// 30000/1001 is 29.97, and a builder that read it as 30000 would ask for a
	// GOP of 120000 frames.
	if m.FrameRate < 29.9 || m.FrameRate > 30.0 {
		t.Errorf("frame rate %v, want ~29.97", m.FrameRate)
	}
}

func TestParseProbeRefusesSourcesTheLadderCannotServe(t *testing.T) {
	cases := map[string]string{
		"audio only": `{"streams": [{"codec_type": "audio", "codec_name": "aac"}], "format": {"format_name": "wav"}}`,
		"no streams": `{"streams": [], "format": {"format_name": "mp4"}}`,
		"no size":    `{"streams": [{"codec_type": "video", "codec_name": "h264", "width": 0, "height": 0}], "format": {}}`,
		"not json":   `<html>404</html>`,
	}
	for name, raw := range cases {
		if _, err := ParseProbe([]byte(raw)); err == nil {
			t.Errorf("%s: parsed without complaint, want an error", name)
		}
	}
}

func TestParseProbeSurvivesAMissingFrameRate(t *testing.T) {
	const raw = `{"streams": [{"codec_type": "video", "codec_name": "vp9", "width": 640, "height": 480,
		"avg_frame_rate": "0/0", "r_frame_rate": "0/0", "duration": "12.5"}], "format": {}}`
	m, err := ParseProbe([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.FrameRate != 0 {
		t.Errorf("frame rate %v, want 0 for unknown", m.FrameRate)
	}
	// Duration fell back to the stream's, since the container had none.
	if m.DurationMs != 12500 {
		t.Errorf("duration %dms, want 12500", m.DurationMs)
	}
}

func TestLadderNeverUpscales(t *testing.T) {
	cases := []struct {
		height int
		want   []string
	}{
		{2160, []string{"1080p", "720p", "480p", "360p"}},
		{1080, []string{"1080p", "720p", "480p", "360p"}},
		{720, []string{"720p", "480p", "360p"}},
		{480, []string{"480p", "360p"}},
		{360, []string{"360p"}},
		{240, []string{"240p"}}, // below every rung: one rendition at its own size
	}
	for _, c := range cases {
		got := Ladder(Media{Width: c.height * 16 / 9, Height: c.height, HasVideo: true})
		if len(got) != len(c.want) {
			t.Fatalf("%dp source: %d rungs, want %d (%v)", c.height, len(got), len(c.want), got)
		}
		for i, r := range got {
			if r.Name != c.want[i] {
				t.Errorf("%dp source: rung %d is %q, want %q", c.height, i, r.Name, c.want[i])
			}
			if r.Height > c.height {
				t.Errorf("%dp source: rung %q upscales to %d", c.height, r.Name, r.Height)
			}
		}
	}
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestHLSArgsAlignsSegmentsAcrossRungs(t *testing.T) {
	src := Media{Width: 1280, Height: 720, FrameRate: 25, HasVideo: true, HasAudio: true}
	ladder := Ladder(src)
	args := HLSArgs("/in/source.mp4", "/out/asset", ladder, src)

	// 25 fps and 4-second segments: a keyframe every 100 frames, on both rungs,
	// or a player that switches rung mid-stream stutters.
	if got := argValue(args, "-g"); got != "100" {
		t.Errorf("-g is %q, want 100", got)
	}
	if got := argValue(args, "-keyint_min"); got != "100" {
		t.Errorf("-keyint_min is %q, want 100", got)
	}
	if got := argValue(args, "-sc_threshold"); got != "0" {
		t.Errorf("-sc_threshold is %q: scene cuts would put keyframes at different times per rung", got)
	}

	if got := argValue(args, "-master_pl_name"); got != MasterPlaylist {
		t.Errorf("master playlist is %q, want %q", got, MasterPlaylist)
	}
	if got := argValue(args, "-hls_segment_type"); got != "fmp4" {
		t.Errorf("segment type is %q, want fmp4", got)
	}
	if got := argValue(args, "-hls_time"); got != "4" {
		t.Errorf("segment duration is %q, want 4", got)
	}

	// One -map per rung per stream: the source is decoded once and fanned out.
	if maps := strings.Count(strings.Join(args, " "), "-map 0:v:0"); maps != len(ladder) {
		t.Errorf("%d video maps for %d rungs", maps, len(ladder))
	}
	if got := argValue(args, "-var_stream_map"); got != "v:0,a:0,name:720p v:1,a:1,name:480p v:2,a:2,name:360p" {
		t.Errorf("var_stream_map is %q", got)
	}

	// Every rung is scaled by height with an even width; -1 would round odd and
	// yuv420p would refuse it.
	for i, r := range ladder {
		flag := "-filter:v:" + string(rune('0'+i))
		if got := argValue(args, flag); got != "scale=-2:"+itoa(r.Height) {
			t.Errorf("%s is %q, want scale=-2:%d", flag, got, r.Height)
		}
	}
}

func TestHLSArgsOmitsAudioWhenTheSourceHasNone(t *testing.T) {
	src := Media{Width: 640, Height: 360, FrameRate: 30, HasVideo: true}
	args := HLSArgs("/in/silent.mp4", "/out/a", Ladder(src), src)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "0:a:0") {
		t.Error("mapped an audio stream that does not exist")
	}
	if strings.Contains(joined, "-c:a") {
		t.Error("asked for an audio codec on a silent source")
	}
	// The variant map must not name audio either, or ffmpeg refuses the call.
	if got := argValue(args, "-var_stream_map"); got != "v:0,name:360p" {
		t.Errorf("var_stream_map is %q, want v:0,name:360p", got)
	}
}

func TestHLSArgsDefaultsAnAbsurdFrameRate(t *testing.T) {
	for _, fps := range []float64{0, -5, 100000} {
		src := Media{Width: 640, Height: 360, FrameRate: fps, HasVideo: true}
		args := HLSArgs("/in/x.mp4", "/out/a", Ladder(src), src)
		if got := argValue(args, "-g"); got != "120" {
			t.Errorf("fps %v: -g is %q, want 120 (30fps assumed)", fps, got)
		}
	}
}

func TestFramesAndPosterAreBounded(t *testing.T) {
	args := FramesArgs("/in/x.mp4", "/out/frames", 5, 40)
	if got := argValue(args, "-vf"); got != "fps=1/5" {
		t.Errorf("-vf is %q, want fps=1/5", got)
	}
	if got := argValue(args, "-frames:v"); got != "40" {
		t.Errorf("-frames:v is %q, want 40", got)
	}
	// A caller that passes nothing still gets a bound: an hour of video must
	// not be able to fill a disk with stills.
	def := FramesArgs("/in/x.mp4", "/out/frames", 0, 0)
	if argValue(def, "-frames:v") == "0" || argValue(def, "-vf") == "fps=1/0" {
		t.Error("zero sampling parameters were taken literally")
	}

	poster := PosterArgs("/in/x.mp4", "/out/poster.jpg", 6100)
	if got := argValue(poster, "-ss"); got != "6.100" {
		t.Errorf("-ss is %q, want 6.100", got)
	}
	// The seek must come before -i, or ffmpeg decodes everything up to it.
	ss, in := indexOf(poster, "-ss"), indexOf(poster, "-i")
	if ss > in {
		t.Errorf("-ss at %d is after -i at %d: that decodes the whole file to reach one frame", ss, in)
	}
}

func TestPosterAtSkipsTheOpeningFrame(t *testing.T) {
	cases := map[int64]int64{
		0:       0,      // unknown duration: take the first frame
		30_000:  3_000,  // a tenth of the way in
		600_000: 10_000, // capped, so a long video does not poster a minute in
	}
	for duration, want := range cases {
		if got := PosterAt(duration); got != want {
			t.Errorf("PosterAt(%d) = %d, want %d", duration, got, want)
		}
	}
}

func indexOf(args []string, flag string) int {
	for i, a := range args {
		if a == flag {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestWidthForMatchesWhatFfmpegWrites(t *testing.T) {
	src := Media{Width: 1280, Height: 720, HasVideo: true}
	cases := map[int]int{
		720: 1280,
		480: 854, // 853.33 rounds up to the nearest even, as scale=-2 does
		360: 640,
	}
	for height, want := range cases {
		if got := WidthFor(src, height); got != want {
			t.Errorf("WidthFor(1280x720, %d) = %d, want %d", height, got, want)
		}
	}
	// A 4:3 source, and a degenerate one.
	if got := WidthFor(Media{Width: 640, Height: 480}, 360); got != 480 {
		t.Errorf("WidthFor(640x480, 360) = %d, want 480", got)
	}
	if got := WidthFor(Media{}, 360); got != 0 {
		t.Errorf("WidthFor of a source with no height = %d, want 0", got)
	}
}
