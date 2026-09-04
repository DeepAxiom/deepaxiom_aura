package pipeline

import "fmt"

// Rendition is one rung of the output ladder.
type Rendition struct {
	Name      string
	Height    int
	VideoKbps int
	AudioKbps int
}

// rungs are the standard ladder, tallest first. The bitrates are H.264 at
// roughly what a general-purpose CDN ships: high enough that a talking head
// looks like a person, low enough that a phone on cellular stays on the top
// rung it can afford.
var rungs = []Rendition{
	{Name: "1080p", Height: 1080, VideoKbps: 5000, AudioKbps: 128},
	{Name: "720p", Height: 720, VideoKbps: 2800, AudioKbps: 128},
	{Name: "480p", Height: 480, VideoKbps: 1400, AudioKbps: 96},
	{Name: "360p", Height: 360, VideoKbps: 800, AudioKbps: 96},
}

// Ladder picks the rungs to encode for a source.
//
// It never upscales. Encoding a 480p consultation recording into a 1080p rung
// spends four times the CPU to ship the same picture with more artefacts, and
// the player would pick it. A source shorter than the smallest rung gets one
// rendition at its own height, because a ladder with no rungs is not a ladder.
func Ladder(src Media) []Rendition {
	var out []Rendition
	for _, r := range rungs {
		if r.Height <= src.Height {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		smallest := rungs[len(rungs)-1]
		out = append(out, Rendition{
			Name:      fmt.Sprintf("%dp", src.Height),
			Height:    src.Height,
			VideoKbps: smallest.VideoKbps,
			AudioKbps: smallest.AudioKbps,
		})
	}
	return out
}
