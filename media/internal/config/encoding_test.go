package config

import (
	"strings"
	"testing"
	"time"

	"aura/media/internal/pipeline"
)

func TestANodeThatSaysNothingAboutEncodingEncodesAsBefore(t *testing.T) {
	// The configuration every existing deployment has: none of the new flags.
	c := base(t.TempDir())
	if err := c.Validate(); err != nil {
		t.Fatalf("a 0.1.0 configuration is refused: %v", err)
	}
	if !c.Encoding().IsDefault() {
		t.Errorf("a node with no encoder settings does not get the 0.1.0 encode: %+v", c.Encoding())
	}
	// And the defaults the flags carry are the same thing spelled out.
	c.VideoEncoder, c.RateMode = "libx264", "cbr-ladder"
	if err := c.Validate(); err != nil || !c.Encoding().IsDefault() {
		t.Errorf("the flag defaults are not the 0.1.0 encode: %+v, %v", c.Encoding(), err)
	}
	// No live unless asked for.
	if c.RTMPBind != "" || c.IngestURL() != "" {
		t.Errorf("live is on without --rtmp-bind: %q", c.IngestURL())
	}
}

func TestAnEncoderThatCannotBeBuiltIsRefusedAtStartUp(t *testing.T) {
	dir := t.TempDir()
	ok := []Config{
		{VideoEncoder: "libx264", RateMode: "capped-crf", CRF: 21},
		{VideoEncoder: " libx265 ", RateMode: " capped-crf "}, // an env var with stray spaces
		{VideoEncoder: "h264_nvenc", Preset: "p5"},
		{VideoEncoder: "libsvtav1", RateMode: "capped-crf"},
	}
	for _, o := range ok {
		c := base(dir)
		c.VideoEncoder, c.RateMode, c.Preset, c.CRF = o.VideoEncoder, o.RateMode, o.Preset, o.CRF
		if err := c.Validate(); err != nil {
			t.Errorf("%+v was refused: %v", o, err)
		}
	}
	bad := map[string]Config{
		"--video-encoder": {VideoEncoder: "x264"},
		"--rate-mode":     {RateMode: "crf"},
		"h264_qsv only":   {VideoEncoder: "h264_qsv", RateMode: "capped-crf"},
		"SVT-AV1":         {VideoEncoder: "libsvtav1"},
		"--crf":           {RateMode: "capped-crf", CRF: 99},
		"--preset":        {Preset: "-vf"},
	}
	for mention, b := range bad {
		c := base(dir)
		c.VideoEncoder, c.RateMode, c.Preset, c.CRF = b.VideoEncoder, b.RateMode, b.Preset, b.CRF
		err := c.Validate()
		if err == nil {
			t.Errorf("%+v was accepted", b)
			continue
		}
		// The refusal names the setting, so whoever reads it knows what to change.
		if !strings.Contains(err.Error(), mention) {
			t.Errorf("the refusal of %+v does not mention %s: %q", b, mention, err)
		}
	}
	// What the pipeline is handed is what was configured.
	c := base(dir)
	c.VideoEncoder, c.RateMode, c.Preset, c.CRF = "libx265", "capped-crf", "fast", 24
	want := pipeline.Encoding{VideoEncoder: "libx265", RateMode: pipeline.RateCappedCRF, Preset: "fast", CRF: 24}
	if got := c.Encoding(); got != want {
		t.Errorf("Encoding() = %+v, want %+v", got, want)
	}
}

func TestLiveSettings(t *testing.T) {
	dir := t.TempDir()
	for _, bind := range []string{"1935", "rtmp://0.0.0.0:1935", "0.0.0.0"} {
		c := base(dir)
		c.RTMPBind = bind
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--rtmp-bind") {
			t.Errorf("--rtmp-bind %q: %v", bind, err)
		}
	}
	c := base(dir)
	c.RTMPBind, c.RTMPURL = "0.0.0.0:1935", "https://live.example.com/live"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--rtmp-url") {
		t.Errorf("an https ingest URL: %v", err)
	}
	c = base(dir)
	c.RTMPBind, c.LiveGrace = "0.0.0.0:1935", -time.Second
	if err := c.Validate(); err == nil {
		t.Error("a negative grace was accepted")
	}

	// What a broadcaster is told to push to.
	cases := []struct{ bind, url, want string }{
		// Configured wins, and a trailing slash would double up with the key.
		{"0.0.0.0:1935", "rtmp://live.example.com/live/", "rtmp://live.example.com/live"},
		{"0.0.0.0:1935", "rtmps://live.example.com:443/live", "rtmps://live.example.com:443/live"},
		// Derived: a wildcard bind is not a name anyone can dial, so it falls
		// back to loopback rather than invent one.
		{"0.0.0.0:1935", "", "rtmp://127.0.0.1/live"},
		{":1935", "", "rtmp://127.0.0.1/live"},
		{"[::]:19350", "", "rtmp://127.0.0.1:19350/live"},
		{"10.0.0.5:1935", "", "rtmp://10.0.0.5/live"},
		{"127.0.0.1:19350", "", "rtmp://127.0.0.1:19350/live"},
	}
	for _, tc := range cases {
		c := base(dir)
		c.RTMPBind, c.RTMPURL = tc.bind, tc.url
		if err := c.Validate(); err != nil {
			t.Errorf("bind %q url %q refused: %v", tc.bind, tc.url, err)
		}
		if got := c.IngestURL(); got != tc.want {
			t.Errorf("bind %q url %q: ingest URL %q, want %q", tc.bind, tc.url, got, tc.want)
		}
	}
	if got := base(dir).LiveRoot(); !strings.HasPrefix(got, dir) || got == base(dir).WorkRoot() {
		t.Errorf("live scratch space is %q", got)
	}
}
