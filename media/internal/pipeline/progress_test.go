package pipeline

import (
	"math"
	"strings"
	"testing"
)

// What ffmpeg 8.1 actually wrote to `-progress pipe:1` for a three-rung
// ladder with a frames output: two blocks before anything has left an
// encoder, one mid-encode, and the last.
const progressText = `frame=0
fps=0.00
stream_0_0_q=0.0
bitrate=N/A
total_size=N/A
out_time_us=N/A
out_time_ms=N/A
out_time=N/A
dup_frames=0
drop_frames=0
speed=N/A
progress=continue
frame=112
fps=79.08
stream_0_0_q=-1.0
stream_1_0_q=3.0
bitrate=N/A
total_size=N/A
out_time_us=20000000
out_time_ms=20000000
out_time=00:00:20.000000
dup_frames=0
drop_frames=0
speed=3.16x
progress=continue
frame=998
fps=79.08
bitrate=N/A
total_size=N/A
out_time_us=39920000
out_time_ms=39920000
out_time=00:00:39.920000
dup_frames=0
drop_frames=0
speed=3.16x
progress=end
`

func TestProgressIsParsedFromWhatFfmpegWrites(t *testing.T) {
	var reports []Progress
	ReadProgress(strings.NewReader(progressText), func(p Progress) { reports = append(reports, p) })
	if len(reports) != 3 {
		t.Fatalf("%d reports from three blocks", len(reports))
	}
	// N/A is "not yet", not zero and not an error.
	if reports[0].OutTimeUs >= 0 || reports[0].Done {
		t.Errorf("the first block has no time yet, got %+v", reports[0])
	}
	if got := reports[0].Fraction(40_000); got != 0 {
		t.Errorf("progress before the first frame is %v, want 0", got)
	}
	if reports[1].OutTimeUs != 20_000_000 || reports[1].Frame != 112 || reports[1].Speed != 3.16 {
		t.Errorf("mid-encode block read as %+v", reports[1])
	}
	if got := reports[1].Fraction(40_000); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("20s into a 40s source is %v, want 0.5", got)
	}
	if !reports[2].Done || reports[2].Fraction(40_000) != 1 {
		t.Errorf("the last block is %+v with fraction %v, want done and 1", reports[2], reports[2].Fraction(40_000))
	}
}

func TestProgressNeverClaimsToBeFinishedBeforeFfmpegDoes(t *testing.T) {
	// A probed duration is routinely a frame short of what gets encoded. The
	// fraction must not reach 1 on arithmetic alone: 1 is what tells a
	// consumer to stop polling.
	p := Progress{OutTimeUs: 40_080_000}
	if got := p.Fraction(40_000); got >= 1 {
		t.Errorf("past the probed duration, not done: fraction %v", got)
	}
	// A source with no known duration has no fraction to report.
	if got := (Progress{OutTimeUs: 5_000_000}).Fraction(0); got != 0 {
		t.Errorf("unknown duration gave %v, want 0", got)
	}
	if got := (Progress{OutTimeUs: -1}).Fraction(40_000); got != 0 {
		t.Errorf("unknown time gave %v, want 0", got)
	}
	if got := (Progress{Done: true}).Fraction(0); got != 1 {
		t.Errorf("done is %v, want 1 whatever the duration", got)
	}
}

func TestProgressSurvivesOlderAndStrangerFfmpegs(t *testing.T) {
	// Before 4.1 there was no out_time_us, only the misnamed out_time_ms,
	// which is microseconds.
	parser := NewProgressParser()
	var got Progress
	for _, line := range []string{"frame=10", "out_time_ms=1500000", "out_time=00:00:01.500000", "progress=continue"} {
		if p, ok := parser.Line(line); ok {
			got = p
		}
	}
	if got.OutTimeUs != 1_500_000 {
		t.Errorf("out_time_ms read as %d microseconds, want 1500000", got.OutTimeUs)
	}

	// Only the clock form.
	parser = NewProgressParser()
	for _, line := range []string{"out_time=01:02:03.500000", "progress=continue"} {
		if p, ok := parser.Line(line); ok {
			got = p
		}
	}
	if want := int64(3723.5 * 1e6); got.OutTimeUs != want {
		t.Errorf("clock form read as %d, want %d", got.OutTimeUs, want)
	}

	// Garbage is skipped, not fatal, and does not close a block.
	parser = NewProgressParser()
	for _, line := range []string{"", "no equals sign", "out_time_us=banana", "frame=", "speed=fastx", "=", "out_time=1:2"} {
		if _, ok := parser.Line(line); ok {
			t.Errorf("%q closed a block", line)
		}
	}
	p, ok := parser.Line("progress=continue")
	if !ok || p.OutTimeUs >= 0 {
		t.Errorf("a block of garbage produced %+v", p)
	}

	// A block that says N/A after one that had a number keeps the number: the
	// reported time does not jump back to unknown.
	parser = NewProgressParser()
	parser.Line("out_time_us=7000000")
	parser.Line("progress=continue")
	parser.Line("out_time_us=N/A")
	p, _ = parser.Line("progress=continue")
	if p.OutTimeUs != 7_000_000 {
		t.Errorf("an N/A block reset the time to %d", p.OutTimeUs)
	}
}

func TestTailBufferKeepsOnlyTheEnd(t *testing.T) {
	buf := &tailBuffer{limit: 10}
	for _, chunk := range []string{"0123456789", "abcde", "", "fgh"} {
		if n, err := buf.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if got := buf.String(); got != "89abcdefgh" {
		t.Errorf("kept %q, want the last ten bytes", got)
	}
	// One write larger than the whole limit.
	buf.Write([]byte(strings.Repeat("x", 25) + "END"))
	if got := buf.String(); got != "xxxxxxxEND" {
		t.Errorf("kept %q", got)
	}
}
