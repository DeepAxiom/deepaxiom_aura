package pipeline

import (
	"fmt"
	"math"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// LiveSegmentSeconds is the segment duration of a live ladder. Two seconds,
// not the four a VOD uses: a player holds about three segments before it
// starts, so the segment length is most of the glass-to-glass delay, and four
// would put a live stream more than twelve seconds behind the person in it.
const LiveSegmentSeconds = 2

// RateMode is how each rung is given its bits.
type RateMode string

const (
	// RateCBRLadder is what 0.1.0 did: every rung is told a bitrate and spends
	// it, whether the picture needs it or not. Predictable, and wasteful on a
	// talking head.
	RateCBRLadder RateMode = "cbr-ladder"
	// RateCappedCRF is the per-title mode: every rung is told a quality and a
	// ceiling. A static slide deck comes out at a fraction of the rung's
	// bitrate, an action scene hits the ceiling, and the ceiling is what keeps
	// the rung honest about what a player on a slow link can afford.
	RateCappedCRF RateMode = "capped-crf"
)

// The encoders this build will hand to ffmpeg. The list is short on purpose:
// an encoder is only here if the ladder it produces can be checked, and the
// check is that every rung cuts its segments at the same instants.
const (
	EncoderX264         = "libx264"
	EncoderX265         = "libx265"
	EncoderSVTAV1       = "libsvtav1"
	EncoderNVENC        = "h264_nvenc"
	EncoderQSV          = "h264_qsv"
	EncoderVideoToolbox = "h264_videotoolbox"
)

// Encoding is the operator's choice of encoder. The zero value is exactly what
// 0.1.0 hard-wired — libx264, veryfast, a fixed-bitrate ladder — so a node
// whose configuration says nothing encodes what it always encoded.
type Encoding struct {
	// VideoEncoder is an ffmpeg encoder name. Empty means libx264.
	VideoEncoder string
	// Preset is passed to the encoder as -preset. Empty means the default this
	// package picks for that encoder, which for some of them is to say nothing.
	Preset string
	// RateMode is cbr-ladder or capped-crf. Empty means cbr-ladder.
	RateMode RateMode
	// CRF is the quality target in capped-crf mode. Zero means the encoder's
	// customary default, which is a different number for each of them.
	CRF int
}

// Encoder is the encoder name with the default applied.
func (e Encoding) Encoder() string {
	if e.VideoEncoder == "" {
		return EncoderX264
	}
	return e.VideoEncoder
}

// Mode is the rate mode with the default applied.
func (e Encoding) Mode() RateMode {
	if e.RateMode == "" {
		return RateCBRLadder
	}
	return e.RateMode
}

// IsDefault reports whether this is the encode 0.1.0 did. The worker uses it to
// decide whether falling back to the legacy path would change anything.
func (e Encoding) IsDefault() bool {
	return e.Encoder() == EncoderX264 && e.Mode() == RateCBRLadder &&
		(e.Preset == "" || e.Preset == "veryfast")
}

// Validate refuses a combination this package cannot build correctly. It runs
// at start-up: an encoder name with a typo in it would otherwise be found four
// minutes into the first job, by ffmpeg, in a log nobody is reading.
func (e Encoding) Validate() error {
	switch e.Encoder() {
	case EncoderX264, EncoderX265:
	case EncoderSVTAV1:
		// SVT-AV1 refuses a maximum bitrate outside CRF mode, so the fixed
		// ladder's maxrate/bufsize pair cannot be expressed for it. Rather than
		// ship an AV1 ladder with no ceiling on any rung, the mode is not
		// offered.
		if e.Mode() != RateCappedCRF {
			return fmt.Errorf("--video-encoder %s only supports --rate-mode %s: SVT-AV1 takes a bitrate ceiling in CRF mode and nowhere else", EncoderSVTAV1, RateCappedCRF)
		}
	case EncoderNVENC, EncoderQSV, EncoderVideoToolbox:
		// Each hardware encoder spells constant quality differently (-cq, ICQ
		// via -global_quality, -q:v), and none of those spellings could be run
		// on the machines this was written on. A mode that has never produced
		// a frame is not one to offer.
		if e.Mode() != RateCBRLadder {
			return fmt.Errorf("--video-encoder %s only supports --rate-mode %s: constant quality on a hardware encoder is spelled differently by each vendor and has not been verified here", e.Encoder(), RateCBRLadder)
		}
	default:
		return fmt.Errorf("--video-encoder %q is not one this build offers (%s)", e.VideoEncoder, strings.Join(Encoders(), ", "))
	}
	switch e.Mode() {
	case RateCBRLadder, RateCappedCRF:
	default:
		return fmt.Errorf("--rate-mode %q is not a rate mode (%s, %s)", e.RateMode, RateCBRLadder, RateCappedCRF)
	}
	if e.CRF < 0 || e.CRF > 63 {
		return fmt.Errorf("--crf %d is outside 0..63", e.CRF)
	}
	if e.Preset != "" && strings.ContainsAny(e.Preset, " \t-") {
		// A preset reaches argv verbatim. It is not shell, so this is not an
		// injection, but "-something" in that position would be read by ffmpeg
		// as the next option rather than as a value.
		return fmt.Errorf("--preset %q is not a preset name", e.Preset)
	}
	return nil
}

// Encoders lists what Validate accepts, for the help text and the refusal.
func Encoders() []string {
	return []string{EncoderX264, EncoderX265, EncoderSVTAV1, EncoderNVENC, EncoderQSV, EncoderVideoToolbox}
}

// crf is the quality target for capped-crf, with each encoder's customary
// default: the scales are not comparable, and 23 on SVT-AV1 would be a file
// several times the size of 23 on x264.
func (e Encoding) crf() int {
	if e.CRF > 0 {
		return e.CRF
	}
	switch e.Encoder() {
	case EncoderX265:
		return 26
	case EncoderSVTAV1:
		return 35
	default:
		return 23
	}
}

// Ladder adjusts a ladder's bitrates for the codec. The standard rungs are
// H.264 numbers; HEVC and AV1 reach the same picture with fewer bits, and
// spending H.264 bitrates on them would throw away the only reason to have
// chosen them.
func (e Encoding) Ladder(ladder []Rendition) []Rendition {
	percent := 100
	switch e.Encoder() {
	case EncoderX265:
		percent = 65
	case EncoderSVTAV1:
		percent = 55
	}
	if percent == 100 {
		return ladder
	}
	out := make([]Rendition, len(ladder))
	for i, r := range ladder {
		r.VideoKbps = r.VideoKbps * percent / 100
		out[i] = r
	}
	return out
}

// forcedKeyframes asks ffmpeg itself for a keyframe at every multiple of the
// segment duration. It is the encoder-independent half of alignment: -g counts
// frames and each encoder interprets it in its own way, but this marks the
// same decoded frames as keyframes for every rung before any encoder sees them.
func forcedKeyframes(segmentSeconds int) []string {
	return []string{"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segmentSeconds)}
}

// videoArgs is the encoder part of the argv, shared by every rung.
//
// For libx264 in cbr-ladder mode this is byte for byte what 0.1.0 passed, and
// TestDefaultEncodingIsTheArgvThatShipped holds it there.
func (e Encoding) videoArgs(gop, segmentSeconds int, live bool) []string {
	g := strconv.Itoa(gop)
	preset := e.Preset
	var args []string
	switch e.Encoder() {
	case EncoderX265:
		if preset == "" {
			preset = "veryfast"
		}
		args = []string{
			"-c:v", EncoderX265,
			"-preset", preset,
			// hvc1, not the hev1 ffmpeg writes by default: Apple's players
			// refuse hev1 in fMP4.
			"-tag:v", "hvc1",
			"-pix_fmt", "yuv420p",
			"-g", g,
			"-keyint_min", g,
			// -sc_threshold is libx264's. x265 has its own switch for the same
			// thing, and open GOPs would let a segment reference the one
			// before it.
			"-x265-params", "scenecut=0:open-gop=0:log-level=error",
		}
		args = append(args, forcedKeyframes(segmentSeconds)...)
	case EncoderSVTAV1:
		if preset == "" {
			preset = "10"
		}
		args = []string{
			"-c:v", EncoderSVTAV1,
			"-preset", preset,
			"-pix_fmt", "yuv420p",
			"-g", g,
			"-svtav1-params", "scd=0",
		}
	case EncoderNVENC:
		args = []string{"-c:v", EncoderNVENC}
		if preset != "" {
			args = append(args, "-preset", preset)
		}
		args = append(args,
			"-profile:v", "main",
			"-pix_fmt", "yuv420p",
			"-g", g,
			"-keyint_min", g,
			"-no-scenecut", "1",
			"-strict_gop", "1",
			"-forced-idr", "1",
		)
		args = append(args, forcedKeyframes(segmentSeconds)...)
	case EncoderQSV:
		args = []string{"-c:v", EncoderQSV}
		if preset != "" {
			args = append(args, "-preset", preset)
		}
		args = append(args,
			"-profile:v", "main",
			// Quick Sync takes NV12 from system memory, not planar 4:2:0.
			"-pix_fmt", "nv12",
			"-g", g,
			"-keyint_min", g,
			"-forced_idr", "1",
		)
		args = append(args, forcedKeyframes(segmentSeconds)...)
	case EncoderVideoToolbox:
		// No -preset: VideoToolbox has none, and passing one fails the call.
		args = []string{
			"-c:v", EncoderVideoToolbox,
			"-profile:v", "main",
			"-pix_fmt", "yuv420p",
			"-g", g,
		}
		args = append(args, forcedKeyframes(segmentSeconds)...)
	default:
		if preset == "" {
			preset = "veryfast"
		}
		args = []string{
			"-c:v", EncoderX264,
			"-preset", preset,
		}
		if live {
			// No lookahead and no B-frames: the encoder stops holding frames
			// back, which is latency a live viewer would otherwise pay on top
			// of the segments.
			args = append(args, "-tune", "zerolatency")
		}
		args = append(args,
			"-profile:v", "main",
			"-pix_fmt", "yuv420p",
			"-g", g,
			"-keyint_min", g,
			"-sc_threshold", "0",
		)
		if live {
			// A broadcaster's frame rate is whatever their encoder felt like,
			// and the probe saw one second of it. Time-based keyframes hold
			// the segment boundary where the frame count would drift.
			args = append(args, forcedKeyframes(segmentSeconds)...)
		}
	}
	if e.Mode() == RateCappedCRF {
		args = append(args, "-crf", strconv.Itoa(e.crf()))
	}
	return args
}

// rungArgs is one rendition's scale and rate control.
func (e Encoding) rungArgs(i int, r Rendition, hasAudio bool) []string {
	idx := strconv.Itoa(i)
	// scale=-2:H keeps the aspect ratio and lands on an even width, which
	// yuv420p requires; -1 would round to odd and fail the encode.
	args := []string{"-filter:v:" + idx, fmt.Sprintf("scale=-2:%d", r.Height)}
	if e.Mode() == RateCappedCRF {
		// The rung's bitrate becomes its ceiling rather than its target. The
		// ceiling is what ffmpeg reports as BANDWIDTH, so a player still
		// chooses rungs by a number that is never exceeded for long.
		args = append(args,
			"-maxrate:v:"+idx, fmt.Sprintf("%dk", r.VideoKbps),
			"-bufsize:v:"+idx, fmt.Sprintf("%dk", r.VideoKbps*2),
		)
	} else {
		args = append(args,
			"-b:v:"+idx, fmt.Sprintf("%dk", r.VideoKbps),
			"-maxrate:v:"+idx, fmt.Sprintf("%dk", r.VideoKbps*11/10),
			"-bufsize:v:"+idx, fmt.Sprintf("%dk", r.VideoKbps*2),
		)
	}
	if hasAudio {
		args = append(args, "-b:a:"+idx, fmt.Sprintf("%dk", r.AudioKbps))
	}
	return args
}

// HLSJob describes one ffmpeg invocation that writes an fMP4 HLS ladder. The
// zero value of everything optional is the 0.1.0 call; each field that is set
// adds one thing to it.
type HLSJob struct {
	Input    string
	OutDir   string
	Ladder   []Rendition
	Source   Media
	Encoding Encoding

	// Progressive writes EVENT playlists that grow a segment at a time and are
	// closed with ENDLIST, instead of VOD playlists written once at the end.
	// It is what lets the worker publish while ffmpeg is still running. It
	// also turns on -progress, so the caller can say how far along it is.
	Progressive bool

	// FramesDir, when set, adds the still-frame sampler as a second output of
	// this same invocation, so the source is decoded once rather than twice.
	FramesDir  string
	FrameEvery float64
	MaxFrames  int

	// Live reads FLV from stdin instead of a file and cuts short segments.
	// Implies Progressive.
	Live bool
}

func (j HLSJob) segmentSeconds() int {
	if j.Live {
		return LiveSegmentSeconds
	}
	return SegmentSeconds
}

// Args is the argv.
func (j HLSJob) Args() []string {
	src := j.Source
	fps := src.FrameRate
	if fps <= 0 || fps > 240 {
		fps = 30
	}
	seg := j.segmentSeconds()
	gop := int(math.Round(fps * float64(seg)))
	if gop < 1 {
		gop = 1
	}
	progressive := j.Progressive || j.Live

	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
		"-y",
	}
	if progressive {
		// key=value blocks on stdout, one a second. -nostats keeps the
		// human-readable copy of the same thing out of stderr, which is the
		// stream the failure message is cut from.
		args = append(args, "-progress", "pipe:1", "-nostats")
	}
	if j.Live {
		// -nostdin stops ffmpeg treating stdin as a keyboard; it does not stop
		// it being an input. One second of analysis, not the default five: the
		// stream arrives in real time, so every second spent deciding what it
		// is is a second before the first segment exists.
		args = append(args, "-analyzeduration", "1000000", "-f", "flv", "-i", "pipe:0")
	} else {
		args = append(args, "-i", j.Input)
	}
	for range j.Ladder {
		args = append(args, "-map", "0:v:0")
		if src.HasAudio {
			args = append(args, "-map", "0:a:0")
		}
	}
	args = append(args, j.Encoding.videoArgs(gop, seg, j.Live)...)
	if src.HasAudio {
		args = append(args, "-c:a", "aac", "-ar", "48000", "-ac", "2")
	}
	for i, r := range j.Ladder {
		args = append(args, j.Encoding.rungArgs(i, r, src.HasAudio)...)
	}

	playlistType, flags := "vod", "independent_segments"
	if progressive {
		// temp_file makes ffmpeg write each playlist and segment beside its
		// final name and rename it into place, so a reader polling the
		// directory never opens one that is half there.
		playlistType, flags = "event", "independent_segments+temp_file"
	}
	args = append(args,
		"-var_stream_map", VarStreamMap(j.Ladder, src.HasAudio),
		"-master_pl_name", MasterPlaylist,
		"-f", "hls",
		"-hls_time", strconv.Itoa(seg),
		"-hls_playlist_type", playlistType,
		"-hls_segment_type", "fmp4",
		"-hls_flags", flags,
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", path.Join(filepath.ToSlash(j.OutDir), "%v", "seg_%05d.m4s"),
		path.Join(filepath.ToSlash(j.OutDir), "%v", "index.m3u8"),
	)

	if j.FramesDir != "" {
		every, max := j.FrameEvery, j.MaxFrames
		if every <= 0 {
			every = 5
		}
		if max <= 0 {
			max = 200
		}
		// A second output on the same input: ffmpeg decodes the source once
		// and hands each frame to both. Output options apply to the output
		// that follows them, so none of the ladder's settings leak in here.
		args = append(args,
			"-map", "0:v:0",
			"-vf", fmt.Sprintf("fps=1/%g", every),
			"-frames:v", strconv.Itoa(max),
			"-q:v", "3",
			path.Join(filepath.ToSlash(j.FramesDir), "frame_%05d.jpg"),
		)
	}
	return args
}
