package pipeline

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// Progress is one report from `ffmpeg -progress`: how much output exists so
// far. ffmpeg writes a block of key=value lines about once a second and closes
// each block with a `progress=` line, which is what makes a block complete.
type Progress struct {
	// OutTimeUs is the timestamp of the last frame written, in microseconds.
	// Negative means ffmpeg has not said yet (it prints N/A until the first
	// frame leaves an encoder).
	OutTimeUs int64
	Frame     int64
	// Speed is encode speed as a multiple of real time; 0 when unknown.
	Speed float64
	// Done is true for the last block ffmpeg writes before it exits.
	Done bool
}

// Fraction is how far through a source of the given duration this report is,
// clamped to 0..1.
//
// It stays below 1 until ffmpeg says it is done: the probed duration is a
// container's opinion and is routinely a frame or two short, and a job that
// reports 100% while it is still writing its last segment has told a consumer
// that polling can stop.
func (p Progress) Fraction(durationMs int64) float64 {
	if p.Done {
		return 1
	}
	if durationMs <= 0 || p.OutTimeUs <= 0 {
		return 0
	}
	f := float64(p.OutTimeUs) / (float64(durationMs) * 1000)
	if f > 0.99 {
		return 0.99
	}
	return f
}

// ProgressParser turns the `-progress` stream into reports, one line at a
// time. It is a value with no I/O in it so the parsing can be tested against
// the text ffmpeg actually writes, on a machine with no ffmpeg.
type ProgressParser struct {
	cur Progress
	// exact says whether this block's time came from out_time_us, so the two
	// less precise spellings of the same number do not overwrite it.
	exact bool
}

// NewProgressParser starts with "nothing known".
func NewProgressParser() *ProgressParser {
	return &ProgressParser{cur: Progress{OutTimeUs: -1}}
}

// Line consumes one line. It returns a report and true when the line closed a
// block.
//
// Unknown keys are ignored, and so are values that do not parse: this text is
// not a contract ffmpeg promises to keep, and a progress bar is never worth
// failing an encode over.
func (p *ProgressParser) Line(line string) (Progress, bool) {
	key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok {
		return Progress{}, false
	}
	value = strings.TrimSpace(value)
	switch key {
	case "out_time_us":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			p.cur.OutTimeUs, p.exact = v, true
		}
	case "out_time_ms":
		// Misnamed upstream: despite the suffix this is microseconds too, and
		// has been for as long as the key has existed. It is only used when
		// out_time_us is missing, which is an ffmpeg older than 4.1.
		if !p.exact {
			if v, err := strconv.ParseInt(value, 10, 64); err == nil {
				p.cur.OutTimeUs = v
			}
		}
	case "out_time":
		if !p.exact {
			if us, ok := parseClock(value); ok && us > p.cur.OutTimeUs {
				p.cur.OutTimeUs = us
			}
		}
	case "frame":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			p.cur.Frame = v
		}
	case "speed":
		if v, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64); err == nil {
			p.cur.Speed = v
		}
	case "progress":
		report := p.cur
		report.Done = value == "end"
		// The next block starts from what this one knew. ffmpeg repeats every
		// key each time, but a block that says N/A where the last one had a
		// number should not make the reported time jump back to "unknown".
		p.exact = false
		return report, true
	}
	return Progress{}, false
}

// ReadProgress reads a whole `-progress` stream, calling fn for each block.
// It returns when the stream ends, which is when ffmpeg exits.
func ReadProgress(r io.Reader, fn func(Progress)) {
	parser := NewProgressParser()
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if report, ok := parser.Line(scanner.Text()); ok && fn != nil {
			fn(report)
		}
	}
	// Whatever is left unread must still be drained, or ffmpeg blocks writing
	// to a pipe nobody is emptying and the encode stops with it.
	_, _ = io.Copy(io.Discard, r)
}

// parseClock reads ffmpeg's HH:MM:SS.micro form into microseconds.
func parseClock(s string) (int64, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.ParseInt(parts[0], 10, 64)
	m, err2 := strconv.ParseInt(parts[1], 10, 64)
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil || h < 0 || m < 0 || sec < 0 {
		return 0, false
	}
	return (h*3600+m*60)*1_000_000 + int64(sec*1_000_000+0.5), true
}
