// Package pipeline is the ffmpeg half: what a source is, what ladder to encode
// it into, and the exact argv that does it.
//
// Every command is built by a pure function and run by something else. ffmpeg
// is not installed on most machines that will read this code, and a builder
// that can only be checked by running it is a builder nobody checks — so the
// argv is the unit under test, and the process is a thin runner over it.
package pipeline

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Media is what a probe found in a source file.
type Media struct {
	DurationMs  int64
	Width       int
	Height      int
	FrameRate   float64
	BitrateKbps int
	VideoCodec  string
	AudioCodec  string
	HasVideo    bool
	HasAudio    bool
	FormatName  string
}

// ParseProbe reads `ffprobe -of json -show_format -show_streams` output.
//
// It is lenient about what it does not need and strict about what it does: a
// source with no video stream is an error here rather than a confusing ffmpeg
// failure four minutes into an encode.
func ParseProbe(raw []byte) (Media, error) {
	var probe struct {
		Format struct {
			Duration   string `json:"duration"`
			BitRate    string `json:"bit_rate"`
			FormatName string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			CodecType    string `json:"codec_type"`
			CodecName    string `json:"codec_name"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			AvgFrameRate string `json:"avg_frame_rate"`
			RFrameRate   string `json:"r_frame_rate"`
			Duration     string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Media{}, fmt.Errorf("ffprobe output is not JSON: %w", err)
	}

	m := Media{FormatName: probe.Format.FormatName}
	if secs, err := strconv.ParseFloat(strings.TrimSpace(probe.Format.Duration), 64); err == nil && secs > 0 {
		m.DurationMs = int64(math.Round(secs * 1000))
	}
	if bits, err := strconv.ParseFloat(strings.TrimSpace(probe.Format.BitRate), 64); err == nil && bits > 0 {
		m.BitrateKbps = int(math.Round(bits / 1000))
	}
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			if m.HasVideo {
				continue // the first video stream is the one that gets encoded
			}
			m.HasVideo = true
			m.VideoCodec = s.CodecName
			m.Width, m.Height = s.Width, s.Height
			m.FrameRate = parseRate(s.AvgFrameRate)
			if m.FrameRate == 0 {
				m.FrameRate = parseRate(s.RFrameRate)
			}
			if m.DurationMs == 0 {
				if secs, err := strconv.ParseFloat(strings.TrimSpace(s.Duration), 64); err == nil && secs > 0 {
					m.DurationMs = int64(math.Round(secs * 1000))
				}
			}
		case "audio":
			if m.HasAudio {
				continue
			}
			m.HasAudio = true
			m.AudioCodec = s.CodecName
		}
	}
	if !m.HasVideo {
		return Media{}, fmt.Errorf("no video stream: this subsystem packages video, and %q has none", m.FormatName)
	}
	if m.Width <= 0 || m.Height <= 0 {
		return Media{}, fmt.Errorf("video stream has no usable dimensions (%dx%d)", m.Width, m.Height)
	}
	return m, nil
}

// parseRate reads ffprobe's "30000/1001" rational form. A rate of 0 means
// unknown, which callers treat as "assume 30" rather than as a failure: a
// missing frame rate is normal in some containers and is never worth refusing
// a whole encode over.
func parseRate(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0/0" {
		return 0
	}
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return v
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}
