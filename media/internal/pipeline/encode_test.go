package pipeline

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// golden reads an argv recorded from the 0.1.0 builder, one argument per line.
// The files were produced by running HLSArgs as it stood at the commit before
// any of this was written; they are not regenerated, which is the point.
func golden(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	// A checkout with autocrlf turns the line endings into CRLF; the arguments
	// themselves never contain either character.
	text := strings.ReplaceAll(string(raw), "\r", "")
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

var (
	src720   = Media{Width: 1280, Height: 720, FrameRate: 25, HasVideo: true, HasAudio: true}
	srcMuted = Media{Width: 640, Height: 360, FrameRate: 29.97, HasVideo: true}
)

func TestDefaultEncodingIsTheArgvThatShipped(t *testing.T) {
	// The fallback is only a fallback if it is the thing that was proven. This
	// pins the whole argv, not the handful of flags the older tests look at.
	cases := []struct {
		golden string
		got    []string
	}{
		{"hls_args_0.1.0_720p_audio.txt", HLSArgs("/in/source.mp4", "/out/asset", Ladder(src720), src720)},
		{"hls_args_0.1.0_360p_silent.txt", HLSArgs("/in/silent.mp4", "/out/a", Ladder(srcMuted), srcMuted)},
	}
	for _, c := range cases {
		want := golden(t, c.golden)
		if !slices.Equal(c.got, want) {
			t.Errorf("%s: the default argv changed.\n got: %q\nwant: %q", c.golden, c.got, want)
		}
	}
	// And spelling the defaults out is the same as leaving them unset.
	explicit := HLSJob{
		Input: "/in/source.mp4", OutDir: "/out/asset", Ladder: Ladder(src720), Source: src720,
		Encoding: Encoding{VideoEncoder: "libx264", Preset: "veryfast", RateMode: RateCBRLadder},
	}.Args()
	if !slices.Equal(explicit, golden(t, "hls_args_0.1.0_720p_audio.txt")) {
		t.Errorf("libx264/veryfast/cbr-ladder named explicitly is not the default argv: %q", explicit)
	}
}

// ladderPart is the encode itself: from the first -map up to the HLS muxer's
// options. It is what must not change between the legacy call and the
// progressive one.
func ladderPart(t *testing.T, args []string) []string {
	t.Helper()
	from, to := indexOf(args, "-map"), indexOf(args, "-var_stream_map")
	if from < 0 || to < from {
		t.Fatalf("no ladder in %q", args)
	}
	return args[from:to]
}

func TestProgressiveJobChangesHowItIsPublishedNotWhatIsEncoded(t *testing.T) {
	ladder := Ladder(src720)
	legacy := HLSArgs("/in/source.mp4", "/out/asset", ladder, src720)
	job := HLSJob{
		Input: "/in/source.mp4", OutDir: "/out/asset", Ladder: ladder, Source: src720,
		Progressive: true, FramesDir: "/out/frames", FrameEvery: 5, MaxFrames: 40,
	}
	args := job.Args()

	if !slices.Equal(ladderPart(t, args), ladderPart(t, legacy)) {
		t.Errorf("the encode differs from 0.1.0's.\n got: %q\nwant: %q", ladderPart(t, args), ladderPart(t, legacy))
	}
	// EVENT, so the playlist is appended to and can be read while it grows;
	// ffmpeg still closes it with ENDLIST when it exits.
	if got := argValue(args, "-hls_playlist_type"); got != "event" {
		t.Errorf("playlist type is %q, want event", got)
	}
	// temp_file is the atomic rename. Without it a poll can read half a playlist.
	if got := argValue(args, "-hls_flags"); got != "independent_segments+temp_file" {
		t.Errorf("hls flags are %q", got)
	}
	// -progress is a global option and must come before the input.
	if p, in := indexOf(args, "-progress"), indexOf(args, "-i"); p < 0 || p > in {
		t.Errorf("-progress at %d, -i at %d", p, in)
	}
	if got := argValue(args, "-progress"); got != "pipe:1" {
		t.Errorf("-progress is %q, want pipe:1", got)
	}
	if indexOf(args, "-nostats") < 0 {
		t.Error("no -nostats: the stats line would fill the stderr the failure message is cut from")
	}
}

func TestFramesComeFromTheSameInvocationAsTheLadder(t *testing.T) {
	args := HLSJob{
		Input: "/in/source.mp4", OutDir: "/out/asset", Ladder: Ladder(src720), Source: src720,
		Progressive: true, FramesDir: "/out/frames", FrameEvery: 5, MaxFrames: 40,
	}.Args()

	// One input. A second -i would be a second decode, which is the thing the
	// combined call exists to avoid.
	inputs := 0
	for _, a := range args {
		if a == "-i" {
			inputs++
		}
	}
	if inputs != 1 {
		t.Fatalf("%d inputs; the frames must come from the ladder's decode", inputs)
	}
	// The frames are the second output: everything about them comes after the
	// HLS output's path, or ffmpeg would apply it to the ladder.
	hlsOut := indexOf(args, "/out/asset/%v/index.m3u8")
	if hlsOut < 0 {
		t.Fatalf("no HLS output in %q", args)
	}
	tail := args[hlsOut+1:]
	want := []string{"-map", "0:v:0", "-vf", "fps=1/5", "-frames:v", "40", "-q:v", "3", "/out/frames/frame_%05d.jpg"}
	if !slices.Equal(tail, want) {
		t.Errorf("frames output is %q, want %q", tail, want)
	}
	// Same sampler as the legacy second call, so a consumer cannot tell which
	// path produced an asset's frames.
	legacy := FramesArgs("/in/source.mp4", "/out/frames", 5, 40)
	for _, flag := range []string{"-vf", "-frames:v", "-q:v"} {
		if argValue(tail, flag) != argValue(legacy, flag) {
			t.Errorf("%s is %q here and %q in the legacy call", flag, argValue(tail, flag), argValue(legacy, flag))
		}
	}
	// Unbounded sampling is never taken literally.
	def := HLSJob{Input: "/in/x", OutDir: "/o", Ladder: Ladder(src720), Source: src720, FramesDir: "/f"}.Args()
	if got := argValue(def[indexOf(def, "/o/%v/index.m3u8"):], "-frames:v"); got != "200" {
		t.Errorf("default frame cap is %q, want 200", got)
	}
	// And no frames directory means no second output at all.
	none := HLSJob{Input: "/in/x", OutDir: "/o", Ladder: Ladder(src720), Source: src720, Progressive: true}.Args()
	if none[len(none)-1] != "/o/%v/index.m3u8" {
		t.Errorf("an output follows the ladder when no frames were asked for: %q", none[len(none)-1])
	}
}

func TestCappedCRFGivesEveryRungAQualityAndACeiling(t *testing.T) {
	ladder := Ladder(src720)
	args := HLSJob{
		Input: "/in/s.mp4", OutDir: "/out/a", Ladder: ladder, Source: src720,
		Encoding: Encoding{RateMode: RateCappedCRF},
	}.Args()
	if got := argValue(args, "-crf"); got != "23" {
		t.Errorf("-crf is %q, want libx264's customary 23", got)
	}
	for i, r := range ladder {
		idx := itoa(i)
		// A target bitrate beside a CRF would turn it back into the fixed
		// ladder: x264 obeys -b:v and ignores the quality.
		if indexOf(args, "-b:v:"+idx) >= 0 {
			t.Errorf("rung %d has a target bitrate in capped-crf mode", i)
		}
		if got := argValue(args, "-maxrate:v:"+idx); got != itoa(r.VideoKbps)+"k" {
			t.Errorf("rung %d ceiling is %q, want %dk", i, got, r.VideoKbps)
		}
		if got := argValue(args, "-bufsize:v:"+idx); got != itoa(r.VideoKbps*2)+"k" {
			t.Errorf("rung %d buffer is %q, want %dk", i, got, r.VideoKbps*2)
		}
		// Audio is untouched by the video rate mode.
		if got := argValue(args, "-b:a:"+idx); got != itoa(r.AudioKbps)+"k" {
			t.Errorf("rung %d audio bitrate is %q", i, got)
		}
	}
	// The alignment flags are the same ones the fixed ladder has.
	for flag, want := range map[string]string{"-g": "100", "-keyint_min": "100", "-sc_threshold": "0"} {
		if got := argValue(args, flag); got != want {
			t.Errorf("%s is %q, want %s", flag, got, want)
		}
	}
	explicit := HLSJob{Input: "/in/s.mp4", OutDir: "/out/a", Ladder: ladder, Source: src720,
		Encoding: Encoding{RateMode: RateCappedCRF, CRF: 19}}.Args()
	if got := argValue(explicit, "-crf"); got != "19" {
		t.Errorf("-crf is %q, want the configured 19", got)
	}
}

func TestEveryEncoderIsAskedForKeyframesOnTheSegmentBoundary(t *testing.T) {
	// One row per encoder offered. Each must carry something that pins a
	// keyframe to every multiple of the segment duration, in a form that does
	// not depend on how that encoder counts frames.
	const forced = "expr:gte(t,n_forced*4)"
	cases := []struct {
		enc     Encoding
		must    map[string]string
		mustNot []string
	}{
		{
			enc:     Encoding{VideoEncoder: EncoderX265},
			must:    map[string]string{"-c:v": "libx265", "-tag:v": "hvc1", "-g": "100", "-keyint_min": "100", "-force_key_frames": forced, "-x265-params": "scenecut=0:open-gop=0:log-level=error", "-preset": "veryfast"},
			mustNot: []string{"-sc_threshold", "-profile:v"},
		},
		{
			enc:     Encoding{VideoEncoder: EncoderSVTAV1, RateMode: RateCappedCRF},
			must:    map[string]string{"-c:v": "libsvtav1", "-g": "100", "-svtav1-params": "scd=0", "-crf": "35", "-preset": "10"},
			mustNot: []string{"-sc_threshold", "-b:v:0"},
		},
		{
			enc:  Encoding{VideoEncoder: EncoderNVENC},
			must: map[string]string{"-c:v": "h264_nvenc", "-g": "100", "-no-scenecut": "1", "-forced-idr": "1", "-strict_gop": "1", "-force_key_frames": forced, "-profile:v": "main"},
			// No preset unless asked: NVENC's names are not x264's, and
			// "veryfast" is not one of them.
			mustNot: []string{"-preset", "-sc_threshold", "-crf"},
		},
		{
			enc:     Encoding{VideoEncoder: EncoderQSV},
			must:    map[string]string{"-c:v": "h264_qsv", "-g": "100", "-forced_idr": "1", "-force_key_frames": forced, "-pix_fmt": "nv12"},
			mustNot: []string{"-preset", "-sc_threshold"},
		},
		{
			enc:     Encoding{VideoEncoder: EncoderVideoToolbox, Preset: "ignored"},
			must:    map[string]string{"-c:v": "h264_videotoolbox", "-g": "100", "-force_key_frames": forced},
			mustNot: []string{"-preset", "-sc_threshold"},
		},
	}
	for _, c := range cases {
		args := HLSJob{Input: "/in/s.mp4", OutDir: "/out/a", Ladder: Ladder(src720), Source: src720, Encoding: c.enc}.Args()
		for flag, want := range c.must {
			if got := argValue(args, flag); got != want {
				t.Errorf("%s: %s is %q, want %q", c.enc.Encoder(), flag, got, want)
			}
		}
		for _, flag := range c.mustNot {
			if indexOf(args, flag) >= 0 {
				t.Errorf("%s: %s is passed and must not be", c.enc.Encoder(), flag)
			}
		}
		// Whatever the encoder, the muxer is told the same things.
		if got := argValue(args, "-hls_segment_type"); got != "fmp4" {
			t.Errorf("%s: segment type %q", c.enc.Encoder(), got)
		}
	}
	// A configured preset reaches the encoders that have one.
	args := HLSJob{Input: "/i", OutDir: "/o", Ladder: Ladder(src720), Source: src720,
		Encoding: Encoding{VideoEncoder: EncoderNVENC, Preset: "p4"}}.Args()
	if got := argValue(args, "-preset"); got != "p4" {
		t.Errorf("nvenc preset is %q, want p4", got)
	}
}

func TestValidateRefusesWhatCannotBeBuiltCorrectly(t *testing.T) {
	ok := []Encoding{
		{},
		{VideoEncoder: EncoderX264, RateMode: RateCappedCRF, CRF: 20},
		{VideoEncoder: EncoderX265},
		{VideoEncoder: EncoderX265, RateMode: RateCappedCRF},
		{VideoEncoder: EncoderSVTAV1, RateMode: RateCappedCRF},
		{VideoEncoder: EncoderNVENC, Preset: "p5"},
		{VideoEncoder: EncoderQSV},
		{VideoEncoder: EncoderVideoToolbox},
	}
	for _, e := range ok {
		if err := e.Validate(); err != nil {
			t.Errorf("%+v was refused: %v", e, err)
		}
	}
	bad := map[string]Encoding{
		"an encoder that is not offered":       {VideoEncoder: "libvpx-vp9"},
		"a typo":                               {VideoEncoder: "libx246"},
		"a rate mode that does not exist":      {RateMode: "vbr"},
		"SVT-AV1 with a fixed ladder":          {VideoEncoder: EncoderSVTAV1},
		"constant quality on NVENC":            {VideoEncoder: EncoderNVENC, RateMode: RateCappedCRF},
		"constant quality on Quick Sync":       {VideoEncoder: EncoderQSV, RateMode: RateCappedCRF},
		"constant quality on VideoToolbox":     {VideoEncoder: EncoderVideoToolbox, RateMode: RateCappedCRF},
		"a CRF off the scale":                  {RateMode: RateCappedCRF, CRF: 64},
		"a negative CRF":                       {RateMode: RateCappedCRF, CRF: -1},
		"a preset that would read as a flag":   {Preset: "-y"},
		"a preset with a space in it":          {Preset: "very fast"},
		"an encoder name that is another flag": {VideoEncoder: "-vn"},
	}
	for name, e := range bad {
		if err := e.Validate(); err == nil {
			t.Errorf("%s was accepted: %+v", name, e)
		}
	}
	// Everything Encoders() advertises must be something Validate takes in at
	// least one mode, or the help text is offering what the node refuses.
	for _, name := range Encoders() {
		cbr := Encoding{VideoEncoder: name}.Validate()
		crf := Encoding{VideoEncoder: name, RateMode: RateCappedCRF}.Validate()
		if cbr != nil && crf != nil {
			t.Errorf("%s is advertised and refused in every mode", name)
		}
	}
}

func TestIsDefaultIsOnlyTrueOfWhatShipped(t *testing.T) {
	for _, e := range []Encoding{{}, {VideoEncoder: "libx264"}, {Preset: "veryfast"}, {RateMode: RateCBRLadder}} {
		if !e.IsDefault() {
			t.Errorf("%+v is the 0.1.0 encode and IsDefault says otherwise", e)
		}
	}
	for _, e := range []Encoding{{Preset: "slow"}, {RateMode: RateCappedCRF}, {VideoEncoder: EncoderX265}} {
		if e.IsDefault() {
			t.Errorf("%+v is not the 0.1.0 encode and IsDefault says it is", e)
		}
	}
}

func TestLadderBitratesFollowTheCodec(t *testing.T) {
	base := Ladder(src720)
	if got := (Encoding{}).Ladder(base); !slices.Equal(got, base) {
		t.Errorf("the H.264 ladder was changed: %v", got)
	}
	hevc := Encoding{VideoEncoder: EncoderX265}.Ladder(base)
	for i := range base {
		if hevc[i].VideoKbps >= base[i].VideoKbps || hevc[i].VideoKbps < base[i].VideoKbps/2 {
			t.Errorf("HEVC %s is %dk against H.264's %dk", hevc[i].Name, hevc[i].VideoKbps, base[i].VideoKbps)
		}
		if hevc[i].Name != base[i].Name || hevc[i].Height != base[i].Height || hevc[i].AudioKbps != base[i].AudioKbps {
			t.Errorf("more than the video bitrate changed: %+v", hevc[i])
		}
	}
	// The caller's ladder is not modified in place.
	if base[0].VideoKbps != 2800 {
		t.Errorf("the input ladder was mutated: %+v", base[0])
	}
}

func TestLiveArgsReadStdinAndCutShortSegments(t *testing.T) {
	src := Media{Width: 1280, Height: 720, FrameRate: 30, HasVideo: true, HasAudio: true}
	args := HLSJob{OutDir: "/live/out", Ladder: Ladder(src), Source: src, Live: true}.Args()

	if got := argValue(args, "-i"); got != "pipe:0" {
		t.Errorf("input is %q, want pipe:0", got)
	}
	if f, in := indexOf(args, "-f"), indexOf(args, "-i"); f > in || args[f+1] != "flv" {
		t.Errorf("the input format must be given before -i and be flv: %q", args[:in+2])
	}
	if got := argValue(args, "-hls_time"); got != "2" {
		t.Errorf("live segments are %q seconds, want 2", got)
	}
	// 30 fps, 2-second segments.
	if got := argValue(args, "-g"); got != "60" {
		t.Errorf("-g is %q, want 60", got)
	}
	// A broadcaster's real frame rate is not known from one second of it, so
	// the boundary is also pinned by time.
	if got := argValue(args, "-force_key_frames"); got != "expr:gte(t,n_forced*2)" {
		t.Errorf("-force_key_frames is %q", got)
	}
	if got := argValue(args, "-tune"); got != "zerolatency" {
		t.Errorf("-tune is %q, want zerolatency", got)
	}
	// A playlist that only grows and is renamed into place: the same two
	// properties the progressive VOD relies on.
	if got := argValue(args, "-hls_playlist_type"); got != "event" {
		t.Errorf("playlist type %q", got)
	}
	if got := argValue(args, "-hls_flags"); got != "independent_segments+temp_file" {
		t.Errorf("hls flags %q", got)
	}
	// No file path may be taken for an input: there is none.
	if indexOf(args, "") >= 0 {
		t.Errorf("an empty argument was passed: %q", args)
	}
	// The stream key must have no way into this argv: the builder is never
	// given it. What it is given is a directory and a probe.
	for _, a := range args {
		if strings.Contains(a, "rtmp://") {
			t.Errorf("an RTMP URL is in ffmpeg's argv, where `ps` shows it: %q", a)
		}
	}
	// The VOD call is not affected by any of this.
	if got := argValue(HLSArgs("/in/x", "/o", Ladder(src), src), "-tune"); got != "" {
		t.Errorf("the VOD encode gained -tune %q", got)
	}
}
