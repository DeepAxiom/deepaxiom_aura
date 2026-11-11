package pipeline

// These run a real ffmpeg, and skip on a machine that has none — the same
// bargain the Postgres tests make. They exist because the argv tests can only
// say what was asked for. Whether an encoder did as it was asked — a keyframe
// at every segment boundary, on every rung — is a property of the output, and
// it is the property the whole ladder exists to have.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func realRunner(t *testing.T) Runner {
	t.Helper()
	r := DefaultRunner()
	if err := r.Check(); err != nil {
		t.Skip("ffmpeg is not on PATH; skipping the tests that encode for real")
	}
	return r
}

// cutSource makes a short clip with a hard scene cut that does not fall on a
// segment boundary. A scene cut is where an encoder left to itself puts a
// keyframe, and one rung deciding that differently from another is exactly
// how a ladder ends up misaligned.
func cutSource(t *testing.T, r Runner, format string, seconds int) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "source."+format)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=854x480:rate=25:duration=2.6",
		"-f", "lavfi", "-i", "mandelbrot=size=854x480:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]",
		"-map", "[v]", "-map", "2:a",
		"-t", itoa(seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "50",
		"-c:a", "aac", "-ar", "44100",
	}
	if format == "flv" {
		args = append(args, "-f", "flv")
	}
	args = append(args, out)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := r.Run(ctx, args); err != nil {
		t.Fatalf("could not make a test source: %v", err)
	}
	return out
}

func readLadder(t *testing.T, outDir string, ladder []Rendition) map[string]MediaPlaylist {
	t.Helper()
	out := map[string]MediaPlaylist{}
	for _, r := range ladder {
		raw, err := os.ReadFile(filepath.Join(outDir, r.Name, "index.m3u8"))
		if err != nil {
			t.Fatalf("rung %s wrote no playlist: %v", r.Name, err)
		}
		p, err := ParseMediaPlaylist(raw)
		if err != nil {
			t.Fatalf("rung %s: %v", r.Name, err)
		}
		out[r.Name] = p
	}
	return out
}

func TestEveryOfferedSoftwareEncoderProducesAnAlignedLadder(t *testing.T) {
	r := realRunner(t)
	source := cutSource(t, r, "mp4", 13)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	probed, err := r.Probe(ctx, source)
	if err != nil {
		t.Fatal(err)
	}

	// Every encoder and mode Validate accepts that can run without particular
	// hardware. The three hardware encoders are not here because there is no
	// such hardware here; the worker checks their output with the same
	// CheckAligned on every job, and falls back when it fails.
	cases := []Encoding{
		{},
		{RateMode: RateCappedCRF},
		{VideoEncoder: EncoderX265},
		{VideoEncoder: EncoderX265, RateMode: RateCappedCRF},
		{VideoEncoder: EncoderSVTAV1, RateMode: RateCappedCRF},
	}
	for _, enc := range cases {
		name := enc.Encoder() + "/" + string(enc.Mode())
		t.Run(name, func(t *testing.T) {
			if err := enc.Validate(); err != nil {
				t.Fatalf("an encoding under test is not one Validate accepts: %v", err)
			}
			if err := r.HasEncoder(ctx, enc.Encoder()); err != nil {
				t.Skipf("this ffmpeg has no %s", enc.Encoder())
			}
			// What the node does at start-up. An ffmpeg too old to describe
			// this codec in HLS is one the node refuses to start with, so there
			// is no ladder of it to check: ffmpeg 6.1 and AV1 is the known case.
			if err := r.SelfTest(ctx, enc, t.TempDir()); errors.Is(err, ErrNoCodecs) {
				t.Skipf("this ffmpeg cannot package %s for HLS, and the self-test says so: %v", enc.Encoder(), err)
			} else if err != nil {
				t.Fatalf("the start-up self-test failed: %v", err)
			}
			dir := t.TempDir()
			outDir, framesDir := filepath.Join(dir, "out"), filepath.Join(dir, "frames")
			ladder := enc.Ladder(Ladder(probed))
			if len(ladder) < 2 {
				t.Fatalf("the test source gives a %d-rung ladder; alignment needs at least two", len(ladder))
			}
			for _, rung := range ladder {
				if err := os.MkdirAll(filepath.Join(outDir, rung.Name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(framesDir, 0o755); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var reports []Progress
			args := HLSJob{
				Input: source, OutDir: outDir, Ladder: ladder, Source: probed, Encoding: enc,
				Progressive: true, FramesDir: framesDir, FrameEvery: 5, MaxFrames: 10,
			}.Args()
			if err := r.RunProgress(ctx, args, func(p Progress) {
				mu.Lock()
				reports = append(reports, p)
				mu.Unlock()
			}); err != nil {
				t.Fatalf("ffmpeg refused the argv: %v", err)
			}

			playlists := readLadder(t, outDir, ladder)
			if err := CheckAligned(playlists); err != nil {
				t.Errorf("the ladder is not aligned: %v", err)
			}
			for rung, p := range playlists {
				if !p.Ended {
					t.Errorf("rung %s was not closed with ENDLIST", rung)
				}
				// Thirteen seconds in four-second segments. A keyframe at the
				// scene cut that also cut a segment would make it five.
				if len(p.Segments) != 4 {
					t.Errorf("rung %s has %d segments, want 4", rung, len(p.Segments))
				}
				for i, s := range p.Segments[:len(p.Segments)-1] {
					if s.Duration < 3.9 || s.Duration > 4.1 {
						t.Errorf("rung %s segment %d is %.3fs, want 4", rung, i, s.Duration)
					}
				}
			}
			master, err := os.ReadFile(filepath.Join(outDir, MasterPlaylist))
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckMaster(master, ladder); err != nil {
				t.Errorf("the master playlist is not one a player can choose from: %v\n%s", err, master)
			}

			// The frames came out of this same call.
			frames, _ := filepath.Glob(filepath.Join(framesDir, "frame_*.jpg"))
			if len(frames) < 2 {
				t.Errorf("%d frames sampled from 13 seconds at one every 5", len(frames))
			}

			mu.Lock()
			defer mu.Unlock()
			if len(reports) == 0 || !reports[len(reports)-1].Done {
				t.Fatalf("progress did not end with a done report: %+v", reports)
			}
			last := -1.0
			for _, p := range reports {
				f := p.Fraction(probed.DurationMs)
				if f < last {
					t.Errorf("progress went backwards: %v after %v", f, last)
				}
				last = f
			}
		})
	}
}

func TestLegacyArgvStillEncodes(t *testing.T) {
	// The fallback, run for real: it is only worth falling back to if it works.
	r := realRunner(t)
	source := cutSource(t, r, "mp4", 9)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	probed, err := r.Probe(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "out")
	ladder := Ladder(probed)
	for _, rung := range ladder {
		if err := os.MkdirAll(filepath.Join(outDir, rung.Name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Run(ctx, HLSArgs(source, outDir, ladder, probed)); err != nil {
		t.Fatal(err)
	}
	playlists := readLadder(t, outDir, ladder)
	if err := CheckAligned(playlists); err != nil {
		t.Error(err)
	}
	for rung, p := range playlists {
		if !p.Ended || len(p.Segments) != 3 {
			t.Errorf("rung %s: ended=%v, %d segments", rung, p.Ended, len(p.Segments))
		}
	}
}

func TestLiveArgvEncodesFromStdinAndClosesOnEndOfInput(t *testing.T) {
	r := realRunner(t)
	source := cutSource(t, r, "flv", 9)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	probed, err := r.Probe(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	ladder := Ladder(probed)
	for _, rung := range ladder {
		if err := os.MkdirAll(filepath.Join(outDir, rung.Name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	proc, err := r.Start(ctx, HLSJob{OutDir: outDir, Ladder: ladder, Source: probed, Live: true}.Args(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(proc.Stdin, f); err != nil {
		t.Fatalf("ffmpeg stopped reading its input: %v (%v)", err, proc.Wait())
	}
	// End of input is how a live encode is ended. ffmpeg must take it as
	// "finish", not as an error.
	proc.Stdin.Close()
	if err := proc.Wait(); err != nil {
		t.Fatalf("ffmpeg did not exit cleanly at end of input: %v", err)
	}
	select {
	case <-proc.Exited():
	default:
		t.Error("Exited is not closed after Wait returned")
	}
	playlists := readLadder(t, outDir, ladder)
	if err := CheckAligned(playlists); err != nil {
		t.Error(err)
	}
	for rung, p := range playlists {
		if !p.Ended {
			t.Errorf("rung %s: no ENDLIST after end of input", rung)
		}
		if p.TargetDuration != LiveSegmentSeconds {
			t.Errorf("rung %s: target duration %d, want %d", rung, p.TargetDuration, LiveSegmentSeconds)
		}
		// Nine seconds in two-second segments: four whole ones and a tail.
		if len(p.Segments) < 4 || len(p.Segments) > 5 {
			t.Errorf("rung %s: %d segments from 9 seconds", rung, len(p.Segments))
		}
	}
}

func TestAFailedFfmpegSaysWhyAndAStoppedOneSaysSo(t *testing.T) {
	r := realRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := r.RunProgress(ctx, []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-progress", "pipe:1", "-i", filepath.Join(t.TempDir(), "absent.mp4"), "-f", "null", "-"}, nil)
	if err == nil || !strings.Contains(err.Error(), "absent.mp4") {
		t.Errorf("the failure does not carry ffmpeg's own words: %v", err)
	}

	// Cancelled mid-encode: Wait returns, and says it was stopped rather than
	// reporting whatever ffmpeg printed as it was killed.
	stopCtx, stop := context.WithCancel(ctx)
	proc, err := r.Start(stopCtx, []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-progress", "pipe:1", "-re", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "null", "-"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(500*time.Millisecond, stop)
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Errorf("a cancelled encode reported %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Wait did not return after the context was cancelled")
	}

	if err := r.HasEncoder(ctx, "libx264"); err != nil {
		t.Errorf("libx264 not found in an ffmpeg that just encoded with it: %v", err)
	}
	if err := r.HasEncoder(ctx, "no_such_encoder"); err == nil {
		t.Error("an encoder that does not exist was reported present")
	}
}

func TestSelfTestRefusesAnEncoderThatDoesNotWorkHere(t *testing.T) {
	r := realRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if err := r.SelfTest(ctx, Encoding{RateMode: RateCappedCRF}, dir); err != nil {
		t.Errorf("libx264 capped-crf failed its self-test: %v", err)
	}
	// Nothing is left behind.
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("the self-test left %d entries in its scratch space", len(left))
	}
	// An encoder that is offered and is not on this machine — a hardware one,
	// on a box with no such hardware, or one this ffmpeg was built without —
	// is refused with ffmpeg's reason, at start-up, rather than rescued by the
	// fallback on every job.
	for _, enc := range []Encoding{{VideoEncoder: EncoderVideoToolbox}, {VideoEncoder: EncoderNVENC}} {
		err := r.SelfTest(ctx, enc, dir)
		if err == nil {
			t.Logf("%s works on this machine", enc.Encoder())
			continue
		}
		if !strings.Contains(err.Error(), enc.Encoder()) || !strings.Contains(err.Error(), "does not work") {
			t.Errorf("the refusal of %s does not say what was refused: %v", enc.Encoder(), err)
		}
	}
	// And a combination Validate refuses never reaches ffmpeg.
	if err := r.SelfTest(ctx, Encoding{VideoEncoder: "libvpx"}, dir); err == nil {
		t.Error("an encoder that is not offered passed the self-test")
	}
}
