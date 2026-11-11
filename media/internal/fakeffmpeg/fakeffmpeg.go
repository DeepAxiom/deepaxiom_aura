// Package fakeffmpeg lets a test binary stand in for ffmpeg and ffprobe.
//
// The worker and the live manager are mostly about what happens around an
// encode: what is published when, what is done when ffmpeg fails, what is left
// behind. None of that needs a codec, and all of it needs to be tested on
// machines with no ffmpeg — and with failures a real ffmpeg cannot be made to
// produce on demand. So the test binary re-executes itself as "ffmpeg": a
// TestMain calls Main, which does nothing unless the environment says this
// process is the fake, and otherwise reads the argv the real builders built,
// writes the files a real ffmpeg would have written, in the order and at the
// pace it would have written them, and exits.
//
// It reads the same argv as the real thing on purpose. A fake that was told
// what to do through a side channel would keep passing after a builder
// stopped saying it.
//
// Nothing outside tests imports this package.
package fakeffmpeg

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variables that turn a test binary into the fake and say how it
// should misbehave.
const (
	// EnvMode makes the process a fake when set. Its value is a
	// comma-separated list of the faults below; "ok" for none.
	EnvMode = "AURA_FAKE_FFMPEG"
	// EnvLog names a file every invocation appends its argv to, one per line,
	// arguments separated by tabs.
	EnvLog = "AURA_FAKE_FFMPEG_LOG"
	// EnvSegments is how many segments a file encode writes (default 4).
	EnvSegments = "AURA_FAKE_FFMPEG_SEGMENTS"
	// EnvDelayMs is the pause between segments (default 100).
	EnvDelayMs = "AURA_FAKE_FFMPEG_DELAY_MS"
)

// Faults.
const (
	// FailProgressive fails any encode that was asked to report progress,
	// after it has written one segment — so something has been published by
	// the time it dies. The legacy argv has no -progress and succeeds.
	FailProgressive = "fail-progressive"
	// FailAll fails every encode.
	FailAll = "fail-all"
	// Misaligned gives one rung a segment of a different length.
	Misaligned = "misaligned"
	// NoCodecs writes a master playlist with no CODECS attribute.
	NoCodecs = "no-codecs"
	// CrashLive exits non-zero part-way through a live encode, with no ENDLIST.
	CrashLive = "crash-live"
	// ProbeFail makes ffprobe refuse the input.
	ProbeFail = "probe-fail"
	// NoFrames fails the separate frames call and the poster call.
	NoFrames = "no-frames"
)

// Main turns this process into the fake when the environment says so, and
// returns immediately otherwise. Call it first thing in TestMain.
func Main() {
	mode := os.Getenv(EnvMode)
	if mode == "" {
		return
	}
	os.Exit(run(os.Args[1:], mode))
}

func has(mode, fault string) bool {
	for _, m := range strings.Split(mode, ",") {
		if strings.TrimSpace(m) == fault {
			return true
		}
	}
	return false
}

func value(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func contains(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}

func die(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	return 1
}

func run(args []string, mode string) int {
	if logPath := os.Getenv(EnvLog); logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, strings.Join(args, "\t"))
			f.Close()
		}
	}
	switch {
	case contains(args, "-show_format"):
		return probe(mode)
	case contains(args, "-encoders"):
		fmt.Println(" V....D libx264              libx264 H.264")
		return 0
	case value(args, "-f") == "hls" || contains(args, "hls"):
		return encode(args, mode)
	default:
		return stills(args, mode)
	}
}

func probe(mode string) int {
	if has(mode, ProbeFail) {
		return die("Invalid data found when processing input")
	}
	fmt.Print(`{"streams": [
  {"codec_type": "video", "codec_name": "h264", "width": 1280, "height": 720, "avg_frame_rate": "25/1", "r_frame_rate": "25/1"},
  {"codec_type": "audio", "codec_name": "aac"}],
 "format": {"duration": "16.000000", "bit_rate": "1000000", "format_name": "mov,mp4,m4a"}}`)
	return 0
}

// stills is the poster call and the legacy frames call: an output path that
// is either one .jpg or a frame_%05d.jpg pattern.
func stills(args []string, mode string) int {
	if has(mode, NoFrames) {
		return die("simulated: no decodable frame")
	}
	out := args[len(args)-1]
	if strings.Contains(out, "%05d") {
		for i := 1; i <= 3; i++ {
			if err := os.WriteFile(strings.Replace(out, "%05d", fmt.Sprintf("%05d", i), 1), []byte("jpeg"), 0o644); err != nil {
				return die("%v", err)
			}
		}
		return 0
	}
	if err := os.WriteFile(out, []byte("jpeg"), 0o644); err != nil {
		return die("%v", err)
	}
	return 0
}

type ladder struct {
	rungs    []string
	pattern  string // .../%v/index.m3u8
	master   string
	seg      int
	event    bool
	listed   map[string][]string // rung -> EXTINF durations so far
	codecs   bool
	progress bool
}

func (l *ladder) dir(rung string) string {
	return filepath.Dir(strings.Replace(l.pattern, "%v", rung, 1))
}

// closeSegment writes segment i of a rung and, for an EVENT playlist, lists
// it — file first, playlist second, renamed into place, as ffmpeg does.
func (l *ladder) closeSegment(rung string, i int, seconds float64) error {
	dir := l.dir(rung)
	if err := os.WriteFile(filepath.Join(dir, "init_0.mp4"), []byte("init "+rung), 0o644); err != nil {
		return err
	}
	name := fmt.Sprintf("seg_%05d.m4s", i)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("%s segment %d", rung, i)), 0o644); err != nil {
		return err
	}
	l.listed[rung] = append(l.listed[rung], fmt.Sprintf("%.6f", seconds))
	if l.event {
		return l.writePlaylist(rung, false)
	}
	return nil
}

func (l *ladder) writePlaylist(rung string, ended bool) error {
	kind := "VOD"
	if l.event {
		kind = "EVENT"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:%s\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"init_0.mp4\"\n", l.seg, kind)
	for i, d := range l.listed[rung] {
		fmt.Fprintf(&b, "#EXTINF:%s,\nseg_%05d.m4s\n", d, i)
	}
	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return atomicWrite(filepath.Join(l.dir(rung), "index.m3u8"), b.String())
}

func (l *ladder) writeMaster() error {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	for _, r := range l.rungs {
		codecs := `,CODECS="avc1.4d401f,mp4a.40.2"`
		if !l.codecs {
			codecs = ""
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=1280x720%s\n%s/index.m3u8\n\n", codecs, r)
	}
	return atomicWrite(l.master, b.String())
}

func atomicWrite(path, text string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	// Renaming over a file a reader has open fails on Windows, where these
	// tests also run. The reader is done in microseconds; try again.
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
}

func report(l *ladder, us int64, state string) {
	if l.progress {
		fmt.Printf("frame=%d\nout_time_us=%d\nout_time_ms=%d\nspeed=2.0x\nprogress=%s\n", us/40000, us, us, state)
	}
}

func encode(args []string, mode string) int {
	l := &ladder{
		seg:      4,
		event:    value(args, "-hls_playlist_type") == "event",
		listed:   map[string][]string{},
		codecs:   !has(mode, NoCodecs),
		progress: contains(args, "-progress"),
	}
	if s, err := strconv.Atoi(value(args, "-hls_time")); err == nil {
		l.seg = s
	}
	for _, part := range strings.Fields(value(args, "-var_stream_map")) {
		if _, name, ok := strings.Cut(part, "name:"); ok {
			l.rungs = append(l.rungs, name)
		}
	}
	for _, a := range args {
		if strings.HasSuffix(a, "%v/index.m3u8") {
			l.pattern = a
		}
	}
	if len(l.rungs) == 0 || l.pattern == "" {
		return die("fake ffmpeg: this is not an HLS ladder argv: %q", args)
	}
	l.master = filepath.Join(filepath.Dir(filepath.Dir(l.pattern)), value(args, "-master_pl_name"))
	for _, r := range l.rungs {
		// Like the real one: %v is expanded, directories are not created.
		if _, err := os.Stat(l.dir(r)); err != nil {
			return die("Failed to open file '%s': no such directory", l.dir(r))
		}
	}
	// The frames output of the combined call, when there is one.
	for _, a := range args {
		if strings.HasSuffix(a, "frame_%05d.jpg") {
			for i := 1; i <= 3; i++ {
				if err := os.WriteFile(strings.Replace(a, "%05d", fmt.Sprintf("%05d", i), 1), []byte("jpeg"), 0o644); err != nil {
					return die("%v", err)
				}
			}
		}
	}
	if has(mode, FailAll) {
		return die("simulated: every encode fails")
	}
	delay := time.Duration(envInt(EnvDelayMs, 100)) * time.Millisecond
	if value(args, "-i") == "pipe:0" {
		return encodeLive(l, mode)
	}

	total := envInt(EnvSegments, 4)
	for i := 0; i < total; i++ {
		time.Sleep(delay)
		for n, r := range l.rungs {
			seconds := float64(l.seg)
			if has(mode, Misaligned) && n == len(l.rungs)-1 && i == 1 {
				seconds += 0.4
			}
			if err := l.closeSegment(r, i, seconds); err != nil {
				return die("%v", err)
			}
		}
		if i == 0 && l.event {
			if err := l.writeMaster(); err != nil {
				return die("%v", err)
			}
		}
		report(l, int64(i+1)*int64(l.seg)*1_000_000, "continue")
		if has(mode, FailProgressive) && l.progress {
			// Something only this attempt writes, so a test can tell whether
			// what it published was cleared before the next attempt.
			_ = os.WriteFile(filepath.Join(l.dir(l.rungs[0]), "seg_00001.m4s"), []byte("from the attempt that failed"), 0o644)
			l.listed[l.rungs[0]] = append(l.listed[l.rungs[0]], "4.000000")
			_ = l.writePlaylist(l.rungs[0], false)
			time.Sleep(4 * delay)
			return die("simulated: the new invocation fails after publishing a segment")
		}
	}
	for _, r := range l.rungs {
		if err := l.writePlaylist(r, true); err != nil {
			return die("%v", err)
		}
	}
	if err := l.writeMaster(); err != nil {
		return die("%v", err)
	}
	report(l, int64(total)*int64(l.seg)*1_000_000, "end")
	return 0
}

// encodeLive reads FLV from stdin and closes a segment for every tag it is
// handed after the first few, until stdin ends.
func encodeLive(l *ladder, mode string) int {
	in := bufio.NewReader(os.Stdin)
	header := make([]byte, 13)
	if _, err := io.ReadFull(in, header); err != nil || string(header[:3]) != "FLV" {
		return die("pipe:0: Invalid data found when processing input (no FLV header)")
	}
	segments, lastTs := 0, int64(-1)
	for {
		// One FLV tag: 11 bytes of header, the body, 4 bytes of trailer.
		var head [11]byte
		if _, err := io.ReadFull(in, head[:]); err != nil {
			break // end of input: the stream is over
		}
		size := int(head[1])<<16 | int(head[2])<<8 | int(head[3])
		ts := int64(head[7])<<24 | int64(head[4])<<16 | int64(head[5])<<8 | int64(head[6])
		if _, err := io.CopyN(io.Discard, in, int64(size)+4); err != nil {
			break
		}
		if head[0] != 9 {
			continue
		}
		if ts < lastTs {
			return die("pipe:0: non-monotonic timestamp %d after %d", ts, lastTs)
		}
		// A segment for every two seconds of stream time.
		for ts >= int64(segments+1)*int64(l.seg)*1000 {
			for _, r := range l.rungs {
				if err := l.closeSegment(r, segments, float64(l.seg)); err != nil {
					return die("%v", err)
				}
			}
			if segments == 0 {
				if err := l.writeMaster(); err != nil {
					return die("%v", err)
				}
			}
			segments++
			if has(mode, CrashLive) && segments == 2 {
				// One rung a segment ahead of the others, and no ENDLIST:
				// what a killed encoder leaves.
				_ = l.closeSegment(l.rungs[0], segments, float64(l.seg))
				return die("simulated: the live encoder crashed")
			}
		}
		lastTs = ts
	}
	if segments > 0 {
		for _, r := range l.rungs {
			if err := l.writePlaylist(r, true); err != nil {
				return die("%v", err)
			}
		}
	}
	return 0
}
