package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// SelfTest encodes nine seconds of a generated test pattern with an encoding
// and checks what comes out: that ffmpeg accepted the argv, that the rungs
// are aligned, and that the master playlist says what codec they are.
//
// It runs at start-up, and only when the operator has asked for something
// other than the default. The questions it answers cannot be answered any
// other way. Whether a hardware encoder works is a fact about the machine, not
// about the ffmpeg build that lists it. Whether the HLS muxer can spell a
// codec in CODECS is a fact about the ffmpeg version: 6.1 writes AV1 variants
// with no CODECS attribute at all, and 8.1 writes them correctly. Finding
// either out on the first real job would mean every job is encoded twice —
// once by the configured encoder, then again by the fallback — and the node
// would look, from the outside, merely slow.
//
// dir is scratch space; what is written there is removed.
func (r Runner) SelfTest(ctx context.Context, enc Encoding, dir string) error {
	if err := enc.Validate(); err != nil {
		return err
	}
	work, err := os.MkdirTemp(dir, "selftest-*")
	if err != nil {
		return fmt.Errorf("self-test scratch space: %w", err)
	}
	defer os.RemoveAll(work)

	// 640x480 gives a two-rung ladder, which is the least that can be
	// misaligned. No audio: nothing about audio changes with the encoder.
	src := Media{Width: 640, Height: 480, FrameRate: 25, HasVideo: true, DurationMs: 9000}
	ladder := enc.Ladder(Ladder(src))
	for _, rung := range ladder {
		if err := os.MkdirAll(filepath.Join(work, rung.Name), 0o755); err != nil {
			return fmt.Errorf("self-test scratch space: %w", err)
		}
	}
	const pattern = "testsrc2=size=640x480:rate=25:duration=9"
	args := HLSJob{Input: pattern, OutDir: work, Ladder: ladder, Source: src, Encoding: enc, Progressive: true}.Args()
	// The input is a filter graph rather than a file, and has to be
	// introduced as one.
	at := slices.Index(args, "-i")
	args = slices.Insert(args, at, "-f", "lavfi")

	fail := func(err error) error {
		return fmt.Errorf("--video-encoder %s (--rate-mode %s) does not work with this ffmpeg on this machine: %w", enc.Encoder(), enc.Mode(), err)
	}
	if err := r.RunProgress(ctx, args, nil); err != nil {
		return fail(err)
	}
	playlists := map[string]MediaPlaylist{}
	for _, rung := range ladder {
		raw, err := os.ReadFile(filepath.Join(work, rung.Name, "index.m3u8"))
		if err != nil {
			return fail(fmt.Errorf("the %s rung wrote no playlist", rung.Name))
		}
		p, err := ParseMediaPlaylist(raw)
		if err != nil {
			return fail(err)
		}
		// Nine seconds in four-second segments is three. More means keyframes
		// landed somewhere they were not asked for and cut a segment there.
		if len(p.Segments) != 3 || !p.Ended {
			return fail(fmt.Errorf("the %s rung has %d segments from nine seconds in four-second segments", rung.Name, len(p.Segments)))
		}
		playlists[rung.Name] = p
	}
	if err := CheckAligned(playlists); err != nil {
		return fail(err)
	}
	master, err := os.ReadFile(filepath.Join(work, MasterPlaylist))
	if err != nil {
		return fail(fmt.Errorf("ffmpeg wrote no %s", MasterPlaylist))
	}
	if err := CheckMaster(master, ladder); err != nil {
		return fail(err)
	}
	return nil
}
