package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SegmentSeconds is the HLS target duration. Four seconds is the usual
// compromise: short enough that a player switches rung quickly, long enough
// that the segment count for an hour of video stays sane.
const SegmentSeconds = 4

// MasterPlaylist is the file a consumer is handed. Its name is fixed because
// it is half of the interface — an asset in, an address out — and an address
// whose last component varies is one nothing downstream can construct.
const MasterPlaylist = "master.m3u8"

// ProbeArgs builds the ffprobe call ParseProbe reads.
func ProbeArgs(input string) []string {
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-of", "json",
		"-show_format",
		"-show_streams",
		input,
	}
}

// HLSArgs builds the one ffmpeg call that encodes the whole ladder.
//
// One call, not one per rung: the source is decoded once and fanned out to
// every rendition, which is most of why this is minutes rather than tens of
// minutes. The GOP is pinned to the segment duration and scene-cut detection is
// off so that every rung has a keyframe at the same timestamps — without that,
// segments do not line up and a player switching rung mid-stream stutters or
// refuses.
//
// The output directory must already contain one subdirectory per rendition,
// named as the rendition is: ffmpeg expands %v but does not create directories.
func HLSArgs(input, outDir string, ladder []Rendition, src Media) []string {
	fps := src.FrameRate
	if fps <= 0 || fps > 240 {
		fps = 30
	}
	gop := int(math.Round(fps * SegmentSeconds))
	if gop < 1 {
		gop = 1
	}

	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
		"-y",
		"-i", input,
	}
	for range ladder {
		args = append(args, "-map", "0:v:0")
		if src.HasAudio {
			args = append(args, "-map", "0:a:0")
		}
	}
	args = append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-profile:v", "main",
		"-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(gop),
		"-keyint_min", strconv.Itoa(gop),
		"-sc_threshold", "0",
	)
	if src.HasAudio {
		args = append(args, "-c:a", "aac", "-ar", "48000", "-ac", "2")
	}
	for i, r := range ladder {
		idx := strconv.Itoa(i)
		// scale=-2:H keeps the aspect ratio and lands on an even width, which
		// yuv420p requires; -1 would round to odd and fail the encode.
		args = append(args,
			"-filter:v:"+idx, fmt.Sprintf("scale=-2:%d", r.Height),
			"-b:v:"+idx, fmt.Sprintf("%dk", r.VideoKbps),
			"-maxrate:v:"+idx, fmt.Sprintf("%dk", r.VideoKbps*11/10),
			"-bufsize:v:"+idx, fmt.Sprintf("%dk", r.VideoKbps*2),
		)
		if src.HasAudio {
			args = append(args, "-b:a:"+idx, fmt.Sprintf("%dk", r.AudioKbps))
		}
	}
	args = append(args,
		"-var_stream_map", VarStreamMap(ladder, src.HasAudio),
		"-master_pl_name", MasterPlaylist,
		"-f", "hls",
		"-hls_time", strconv.Itoa(SegmentSeconds),
		"-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4",
		"-hls_flags", "independent_segments",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", path.Join(filepath.ToSlash(outDir), "%v", "seg_%05d.m4s"),
		path.Join(filepath.ToSlash(outDir), "%v", "index.m3u8"),
	)
	return args
}

// VarStreamMap names each variant so %v expands to the rendition name rather
// than to an index — a directory called 720p is one an operator can reason
// about in a bucket listing.
func VarStreamMap(ladder []Rendition, hasAudio bool) string {
	var parts []string
	for i, r := range ladder {
		if hasAudio {
			parts = append(parts, fmt.Sprintf("v:%d,a:%d,name:%s", i, i, r.Name))
			continue
		}
		parts = append(parts, fmt.Sprintf("v:%d,name:%s", i, r.Name))
	}
	return strings.Join(parts, " ")
}

// FramesArgs samples still frames for the AI capabilities this subsystem exists
// to serve. They are extracted from the same decode as the encode because every
// one of those capabilities needs frames, and decoding the source a second time
// later costs exactly as much as the first.
//
// everySeconds is the sampling interval; max caps how many are written, so an
// hour-long source cannot fill a disk on its own.
func FramesArgs(input, outDir string, everySeconds float64, max int) []string {
	if everySeconds <= 0 {
		everySeconds = 5
	}
	if max <= 0 {
		max = 200
	}
	return []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
		"-y",
		"-i", input,
		"-vf", fmt.Sprintf("fps=1/%g", everySeconds),
		"-frames:v", strconv.Itoa(max),
		"-q:v", "3",
		path.Join(filepath.ToSlash(outDir), "frame_%05d.jpg"),
	}
}

// PosterArgs grabs one frame for a thumbnail. The seek is before -i so ffmpeg
// jumps to the keyframe instead of decoding everything up to it.
func PosterArgs(input, outPath string, atMs int64) []string {
	if atMs < 0 {
		atMs = 0
	}
	return []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
		"-y",
		"-ss", fmt.Sprintf("%.3f", float64(atMs)/1000),
		"-i", input,
		"-frames:v", "1",
		"-q:v", "2",
		filepath.ToSlash(outPath),
	}
}

// PosterAt is where to take the poster from: a tenth of the way in, capped, so
// it is past the fade-in and not the black frame most videos open on.
func PosterAt(durationMs int64) int64 {
	if durationMs <= 0 {
		return 0
	}
	at := durationMs / 10
	if at > 10_000 {
		at = 10_000
	}
	return at
}

// Runner executes the commands the builders above produce.
type Runner struct {
	FFmpeg  string
	FFprobe string
	// Timeout bounds a single ffmpeg call. Zero means no bound, which is right
	// for a worker whose lease already bounds it and wrong for anything else.
	Timeout time.Duration
}

// DefaultRunner uses whatever ffmpeg is on PATH.
func DefaultRunner() Runner {
	return Runner{FFmpeg: "ffmpeg", FFprobe: "ffprobe"}
}

// Check reports whether both binaries are actually there. A media service
// without ffmpeg can accept uploads and fail every single one of them, so this
// is called at start-up: the failure belongs where an operator is watching, not
// four minutes into the first job.
func (r Runner) Check() error {
	for _, bin := range []string{r.ffmpeg(), r.ffprobe()} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH: this subsystem decodes and encodes, and cannot do either without it", bin)
		}
	}
	return nil
}

// Probe reads what a source is.
func (r Runner) Probe(ctx context.Context, input string) (Media, error) {
	out, err := r.output(ctx, r.ffprobe(), ProbeArgs(input))
	if err != nil {
		return Media{}, err
	}
	return ParseProbe(out)
}

// Run executes ffmpeg with prebuilt args.
func (r Runner) Run(ctx context.Context, args []string) error {
	_, err := r.output(ctx, r.ffmpeg(), args)
	return err
}

func (r Runner) output(ctx context.Context, bin string, args []string) ([]byte, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s was still running after %s and was stopped", filepath.Base(bin), r.Timeout)
	}
	// The tail, not the whole log: ffmpeg's last lines are the ones that say
	// why, and the rest is a per-frame progress report nobody reads.
	return nil, fmt.Errorf("%s failed: %w: %s", filepath.Base(bin), err, tail(stderr.String(), 800))
}

func (r Runner) ffmpeg() string {
	if r.FFmpeg == "" {
		return "ffmpeg"
	}
	return r.FFmpeg
}

func (r Runner) ffprobe() string {
	if r.FFprobe == "" {
		return "ffprobe"
	}
	return r.FFprobe
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
