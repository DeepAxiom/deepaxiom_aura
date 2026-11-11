package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Segment is one entry of a media playlist.
type Segment struct {
	URI      string
	Duration float64
	// info is the EXTINF value exactly as ffmpeg wrote it. A playlist that is
	// rendered again must say the same duration the first one did, and a float
	// that has been through a round trip may not.
	info string
	// tags are any other tag lines that stood in front of this segment
	// (EXT-X-DISCONTINUITY, EXT-X-PROGRAM-DATE-TIME). They are carried rather
	// than understood, so a tag this parser has never heard of survives it.
	tags []string
}

// MediaPlaylist is what a rung's index.m3u8 says, as far as publishing needs
// to know: which files it names, and whether it is finished.
type MediaPlaylist struct {
	Version        int
	TargetDuration int
	MediaSequence  int64
	Independent    bool
	// Init is the EXT-X-MAP URI: the fMP4 initialisation segment every media
	// segment needs before it can be decoded.
	Init     string
	Segments []Segment
	// Ended is whether EXT-X-ENDLIST is present: nothing more will be added.
	Ended bool
	// Truncated is true when the text stopped in the middle of an entry. The
	// entries before the cut are still good; the caller should not treat the
	// text itself as something to hand to a player.
	Truncated bool
}

// ErrNotAPlaylist is returned for bytes that are not HLS at all.
var ErrNotAPlaylist = errors.New("not an HLS playlist")

// ErrNoCodecs is returned by CheckMaster for a variant with no CODECS
// attribute: this ffmpeg's HLS muxer does not know how to describe the codec
// it was just asked to mux.
var ErrNoCodecs = errors.New("the master playlist has a variant with no CODECS attribute; this ffmpeg cannot describe that codec in HLS")

// ParseMediaPlaylist reads a media playlist.
//
// This is the "which segments are complete" rule. ffmpeg lists a segment in
// the playlist only after it has closed the file, so a name that appears here
// is a name whose bytes are all on disk; a segment file that exists but is not
// listed yet is one ffmpeg is still writing, and is never touched.
//
// A segment counts only when both of its lines are whole: the EXTINF and a URI
// line that ends in a newline. A playlist caught mid-write therefore yields
// the entries before the cut and nothing made up from the half-line after it.
func ParseMediaPlaylist(raw []byte) (MediaPlaylist, error) {
	var p MediaPlaylist
	if !bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("#EXTM3U")) {
		return p, ErrNotAPlaylist
	}
	text := string(raw)
	complete := strings.HasSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if !complete {
		// The last element is a line with no newline after it: either ffmpeg
		// was interrupted, or this is a read of a file still being written.
		lines = lines[:len(lines)-1]
		p.Truncated = true
	} else {
		lines = lines[:len(lines)-1]
	}

	var (
		pendingInfo string
		pendingDur  float64
		havePending bool
		pendingTags []string
	)
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if !havePending {
				// A URI with no EXTINF in front of it is a master playlist's
				// variant line, or damage. Either way it is not a segment.
				continue
			}
			p.Segments = append(p.Segments, Segment{URI: line, Duration: pendingDur, info: pendingInfo, tags: pendingTags})
			havePending, pendingTags = false, nil
			continue
		}
		tag, value, _ := strings.Cut(line, ":")
		switch tag {
		case "#EXTM3U":
		case "#EXT-X-VERSION":
			p.Version, _ = strconv.Atoi(value)
		case "#EXT-X-TARGETDURATION":
			p.TargetDuration, _ = strconv.Atoi(value)
		case "#EXT-X-MEDIA-SEQUENCE":
			p.MediaSequence, _ = strconv.ParseInt(value, 10, 64)
		case "#EXT-X-INDEPENDENT-SEGMENTS":
			p.Independent = true
		case "#EXT-X-PLAYLIST-TYPE":
			// Not kept: whether a playlist is EVENT or VOD is decided by whoever
			// renders it, from whether it has ended.
		case "#EXT-X-MAP":
			p.Init = attribute(value, "URI")
		case "#EXT-X-ENDLIST":
			p.Ended = true
		case "#EXTINF":
			dur, _, _ := strings.Cut(value, ",")
			seconds, err := strconv.ParseFloat(strings.TrimSpace(dur), 64)
			if err != nil {
				return p, fmt.Errorf("playlist has an EXTINF that is not a duration: %q", line)
			}
			pendingInfo, pendingDur, havePending = value, seconds, true
		default:
			if strings.HasPrefix(tag, "#EXT") {
				pendingTags = append(pendingTags, line)
			}
		}
	}
	if havePending {
		// An EXTINF whose URI line never arrived.
		p.Truncated = true
	}
	if p.Truncated {
		// ENDLIST is the last thing ffmpeg writes. A text that was cut short
		// cannot have it, and must not be believed if it claims to.
		p.Ended = false
	}
	return p, nil
}

// attribute reads one quoted attribute out of an attribute list.
func attribute(list, name string) string {
	_, rest, ok := strings.Cut(list, name+"=\"")
	if !ok {
		return ""
	}
	value, _, ok := strings.Cut(rest, "\"")
	if !ok {
		return ""
	}
	return value
}

// DurationMs is the total of every listed segment.
func (p MediaPlaylist) DurationMs() int64 {
	var total float64
	for _, s := range p.Segments {
		total += s.Duration
	}
	return int64(math.Round(total * 1000))
}

// Files is every file the playlist needs a player to be able to fetch, the
// initialisation segment first.
func (p MediaPlaylist) Files() []string {
	out := make([]string, 0, len(p.Segments)+1)
	if p.Init != "" {
		out = append(out, p.Init)
	}
	for _, s := range p.Segments {
		out = append(out, s.URI)
	}
	return out
}

// Truncate keeps the first n segments. It is how a ladder whose rungs stopped
// at slightly different points — an encoder killed mid-stream — is cut back to
// what every rung has.
func (p MediaPlaylist) Truncate(n int) MediaPlaylist {
	if n < len(p.Segments) {
		p.Segments = p.Segments[:n]
	}
	return p
}

// Render writes the playlist back out.
//
// window is how many of the most recent segments to list; zero or less means
// all of them. A windowed playlist is the sliding window a live player polls:
// EXT-X-MEDIA-SEQUENCE moves forward by exactly the number of entries dropped
// from the front, which is how a player knows the entry it was about to fetch
// is the same one it saw last time.
//
// ended closes the playlist. A playlist that is whole and closed is marked VOD
// so a player offers a seek bar from zero; one that is whole and still open is
// marked EVENT, which promises that entries are only ever appended; a window
// carries no type at all, because the only type that would be true of it is
// "live", and that is spelled by leaving the tag out.
func (p MediaPlaylist) Render(window int, ended bool) []byte {
	segments := p.Segments
	sequence := p.MediaSequence
	windowed := false
	if window > 0 && len(segments) > window {
		sequence += int64(len(segments) - window)
		segments = segments[len(segments)-window:]
		windowed = true
	}
	version := p.Version
	if version < 7 {
		version = 7 // EXT-X-MAP outside an I-frame playlist needs 6; fMP4 is 7
	}
	target := p.TargetDuration
	for _, s := range p.Segments {
		// Over every segment, not only the ones in the window: the target must
		// never change while the playlist is live.
		if d := int(math.Ceil(s.Duration - 0.0005)); d > target {
			target = d
		}
	}
	if target < 1 {
		target = 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", version)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", sequence)
	switch {
	case ended && !windowed:
		b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	case !windowed && window <= 0:
		b.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	}
	if p.Independent {
		b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	}
	if p.Init != "" {
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", p.Init)
	}
	for _, s := range segments {
		for _, tag := range s.tags {
			b.WriteString(tag)
			b.WriteByte('\n')
		}
		info := s.info
		if info == "" {
			info = fmt.Sprintf("%.6f,", s.Duration)
		}
		fmt.Fprintf(&b, "#EXTINF:%s\n%s\n", info, s.URI)
	}
	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(b.String())
}

// CheckAligned reports whether every rung cut its segments at the same
// instants, which is the property the whole ladder exists to have: a player
// switches rung at a segment boundary, and if segment 12 of one rung does not
// start where segment 12 of another does, the switch stutters or fails.
//
// The builders ask each encoder for aligned keyframes in that encoder's own
// dialect. This is the check that the encoder did as it was asked, on the
// output rather than on the argv, and it is why a hardware encoder that could
// not be run on the machine this was written on can still be offered: a ladder
// it misaligns is caught here and re-encoded by the software path instead of
// being published.
func CheckAligned(rungs map[string]MediaPlaylist) error {
	var refName string
	var ref MediaPlaylist
	for name, p := range rungs {
		if refName == "" || name < refName {
			refName, ref = name, p
		}
	}
	for name, p := range rungs {
		if name == refName {
			continue
		}
		if len(p.Segments) != len(ref.Segments) {
			return fmt.Errorf("rung %s has %d segments and rung %s has %d: the ladder's keyframes are not aligned",
				name, len(p.Segments), refName, len(ref.Segments))
		}
		for i := range p.Segments {
			// Five milliseconds: less than one frame at any rate this service
			// will see, more than the rounding in a six-decimal EXTINF.
			if math.Abs(p.Segments[i].Duration-ref.Segments[i].Duration) > 0.005 {
				return fmt.Errorf("segment %d is %.3fs in rung %s and %.3fs in rung %s: the ladder's keyframes are not aligned",
					i, p.Segments[i].Duration, name, ref.Segments[i].Duration, refName)
			}
		}
	}
	return nil
}

// CheckMaster reports whether a master playlist is one a player can choose
// from: it names every rung, and says what codec each one is.
//
// CODECS is the part that varies. ffmpeg's HLS muxer knows how to spell it for
// H.264 in every version, and for HEVC and AV1 only in newer ones; where it
// does not, it leaves the attribute out, and a player handed an fMP4 variant
// with no CODECS either guesses or refuses. Better to find that here.
func CheckMaster(raw []byte, ladder []Rendition) error {
	text := string(raw)
	if !strings.Contains(text, "#EXTM3U") {
		return ErrNotAPlaylist
	}
	variants := 0
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		variants++
		if attribute(line, "CODECS") == "" {
			return ErrNoCodecs
		}
	}
	if variants != len(ladder) {
		return fmt.Errorf("the master playlist lists %d variants for a ladder of %d", variants, len(ladder))
	}
	for _, r := range ladder {
		if !strings.Contains(text, r.Name+"/index.m3u8") {
			return fmt.Errorf("the master playlist does not list the %s rung", r.Name)
		}
	}
	return nil
}
