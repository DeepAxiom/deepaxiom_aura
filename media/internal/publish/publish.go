// Package publish moves an HLS package into the store while ffmpeg is still
// writing it.
//
// 0.1.0 uploaded the whole tree after the encode finished, so an hour of video
// was unplayable for as long as it took to encode an hour of video. This
// uploads each segment as soon as it is closed and each playlist as soon as it
// changes, so the address works a few seconds after the encode starts. The
// same mechanism is the whole of live: a live stream is an encode that
// publishes as it goes and does not know when it will end.
//
// Two rules do the work, and both are about order.
//
// A segment is uploaded only once the rung's playlist lists it. ffmpeg adds a
// segment to the playlist after closing the file, so a listed segment is a
// complete one; the file ffmpeg is still writing exists on disk but is not
// listed, and is never read.
//
// A playlist is uploaded only after every file it names. What is in the store
// is then always consistent from a player's side: it can never be told about a
// segment it cannot fetch.
package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"aura/media/internal/pipeline"
	"aura/media/internal/store"
)

// RungPlaylist is the name of each rendition's media playlist.
const RungPlaylist = "index.m3u8"

// DVRPlaylist is the whole stream so far, written beside a windowed live
// playlist. The window is what a live player polls; this is what lets a viewer
// scroll back to the start, and what a node finalising a stream it did not
// encode reads to find out what the stream contained.
const DVRPlaylist = "dvr.m3u8"

// Publisher syncs one local HLS tree into the store. It is not safe for
// concurrent use: one goroutine calls Sync on a timer and then Finish.
type Publisher struct {
	Store store.Store
	// Dir is the local tree ffmpeg writes: master.m3u8 and one directory per
	// rung.
	Dir string
	// Prefix is the store key the tree is published under.
	Prefix string
	// Rungs are the rendition directory names.
	Rungs []string

	// Window, when positive, is how many segments the published playlist
	// lists. The playlists are then rendered here rather than copied, and the
	// full list is published beside each as dvr.m3u8. Zero publishes ffmpeg's
	// own playlists byte for byte.
	Window int
	// DropLocal removes a segment from local disk once it is in the store. A
	// VOD job's work directory is deleted when the job ends; a live stream's
	// would otherwise grow for as long as someone keeps broadcasting.
	DropLocal bool

	// sent is every key that is in the store, with the size that was put.
	sent map[string]int64
	// playlists is the last text published under each playlist key, so an
	// unchanged playlist is not uploaded again twice a second.
	playlists map[string][]byte
	// last is the most recent complete parse of each rung's local playlist.
	last map[string]pipeline.MediaPlaylist
	// master is whether the master playlist is in the store.
	master bool
	// final is set once ffmpeg has exited. Until then a playlist that cannot
	// be read is one ffmpeg is in the middle of replacing, and the answer is
	// to look again on the next pass; after it, nobody else is writing and a
	// read that fails is a real failure.
	final bool
}

// Status is what one Sync found.
type Status struct {
	// Playable is true once the master playlist and at least one segment of
	// every rung are in the store: the address can be handed out.
	Playable bool
	// Segments is how many segments every rung has published.
	Segments int
	// DurationMs is the published length, by the first rung's playlist.
	DurationMs int64
	// Ended is true when every rung's local playlist carries ENDLIST.
	Ended bool
}

func (p *Publisher) init() {
	if p.sent == nil {
		p.sent = map[string]int64{}
		p.playlists = map[string][]byte{}
		p.last = map[string]pipeline.MediaPlaylist{}
	}
}

// Sync uploads whatever became complete since the last call. It is cheap when
// nothing changed and is meant to be called a couple of times a second.
func (p *Publisher) Sync(ctx context.Context) (Status, error) {
	p.init()
	for _, rung := range p.Rungs {
		if err := p.syncRung(ctx, rung); err != nil {
			return Status{}, err
		}
	}
	status := p.status()
	if status.Segments == 0 {
		return status, nil
	}
	// The master playlist goes last, and only once every rung it names has a
	// playlist a player can follow. It is the address: publishing it is what
	// makes the asset playable, so nothing it points at may be missing.
	if err := p.syncMaster(ctx); err != nil {
		return Status{}, err
	}
	status.Playable = p.master
	return status, nil
}

func (p *Publisher) status() Status {
	status := Status{Ended: len(p.Rungs) > 0}
	for i, rung := range p.Rungs {
		pl, ok := p.last[rung]
		if !ok {
			return Status{}
		}
		if i == 0 || len(pl.Segments) < status.Segments {
			status.Segments = len(pl.Segments)
		}
		if !pl.Ended {
			status.Ended = false
		}
	}
	if len(p.Rungs) > 0 {
		status.DurationMs = p.last[p.Rungs[0]].Truncate(status.Segments).DurationMs()
	}
	return status
}

func (p *Publisher) syncRung(ctx context.Context, rung string) error {
	raw, err := os.ReadFile(filepath.Join(p.Dir, rung, RungPlaylist))
	if errors.Is(err, os.ErrNotExist) {
		return nil // ffmpeg has not closed this rung's first segment yet
	}
	if err != nil {
		if !p.final {
			// On Windows a file being renamed into place cannot be opened for
			// the instant it takes, and ffmpeg replaces this one every few
			// seconds. Found by running the tests there: a live stream was
			// being ended over it. Nothing has been lost; the next pass reads
			// it.
			return nil
		}
		return fmt.Errorf("reading the %s playlist: %w", rung, err)
	}
	pl, err := pipeline.ParseMediaPlaylist(raw)
	if err != nil || pl.Truncated {
		// Caught mid-write, or not a playlist yet. ffmpeg renames playlists
		// into place so this should not happen; if it does, the next pass
		// reads a whole one, and nothing was uploaded on the strength of half.
		return nil
	}
	for _, name := range pl.Files() {
		if err := p.putFile(ctx, rung, name); err != nil {
			return err
		}
	}
	p.last[rung] = pl

	if p.Window > 0 {
		// Rendered, because ffmpeg wrote the whole list and a live viewer is
		// given only the end of it. The whole list is published too.
		if err := p.putPlaylist(ctx, path.Join(p.Prefix, rung, DVRPlaylist), pl.Render(0, false)); err != nil {
			return err
		}
		return p.putPlaylist(ctx, path.Join(p.Prefix, rung, RungPlaylist), pl.Render(p.Window, false))
	}
	// ffmpeg's own bytes, unchanged: this is the path every VOD takes, and the
	// playlist a player reads should be the one the muxer wrote.
	return p.putPlaylist(ctx, path.Join(p.Prefix, rung, RungPlaylist), raw)
}

// putFile uploads one file a playlist names, once.
func (p *Publisher) putFile(ctx context.Context, rung, name string) error {
	if strings.ContainsAny(name, "/\\") || name == "" {
		// The playlists this reads were written by ffmpeg from an argv built in
		// this module, and name their files without directories. Anything else
		// is not ours to follow out of the rung's directory.
		return fmt.Errorf("the %s playlist names %q, which is not a file beside it", rung, name)
	}
	key := path.Join(p.Prefix, rung, name)
	if _, done := p.sent[key]; done {
		return nil
	}
	local := filepath.Join(p.Dir, rung, name)
	f, err := os.Open(local)
	if err != nil {
		return fmt.Errorf("the %s playlist lists %s, which is not on disk: %w", rung, name, err)
	}
	obj, err := p.Store.Put(ctx, key, f)
	f.Close()
	if err != nil {
		return err
	}
	p.sent[key] = obj.Bytes
	if p.DropLocal && strings.HasPrefix(name, "seg_") {
		_ = os.Remove(local)
	}
	return nil
}

func (p *Publisher) putPlaylist(ctx context.Context, key string, text []byte) error {
	if bytes.Equal(p.playlists[key], text) {
		return nil
	}
	if _, err := p.Store.Put(ctx, key, bytes.NewReader(text)); err != nil {
		return err
	}
	p.playlists[key] = text
	return nil
}

func (p *Publisher) syncMaster(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(p.Dir, pipeline.MasterPlaylist))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		if !p.final {
			return nil // being replaced; see syncRung
		}
		return fmt.Errorf("reading the master playlist: %w", err)
	}
	if !bytes.HasPrefix(raw, []byte("#EXTM3U")) || !bytes.HasSuffix(raw, []byte("\n")) {
		return nil
	}
	for _, rung := range p.Rungs {
		if !bytes.Contains(raw, []byte(rung+"/"+RungPlaylist)) {
			return nil // still being written, or not this ladder's
		}
	}
	if p.Window > 0 {
		// The same ladder, pointed at the whole-stream playlists.
		dvr := bytes.ReplaceAll(raw, []byte("/"+RungPlaylist), []byte("/"+DVRPlaylist))
		if err := p.putPlaylist(ctx, path.Join(p.Prefix, DVRPlaylist), dvr); err != nil {
			return err
		}
	}
	if err := p.putPlaylist(ctx, path.Join(p.Prefix, pipeline.MasterPlaylist), raw); err != nil {
		return err
	}
	p.master = true
	return nil
}

// Result is what a finished package contains.
type Result struct {
	Segments   int
	DurationMs int64
	// Playlists are the final media playlists, by rung.
	Playlists map[string]pipeline.MediaPlaylist
}

// Finish closes the package: a last sync, the final playlists, and a check
// that every file they name is in the store at the size it has on disk.
//
// clean says whether ffmpeg exited the way it should. When it did, the
// playlists it wrote are the truth and must be closed with ENDLIST, or this
// fails. When it did not — a live encoder that crashed, or was killed because
// the node is stopping — what was published so far is still a valid, shorter
// video, and the playlists are closed here: cut back to the segments every
// rung has, and ended.
func (p *Publisher) Finish(ctx context.Context, clean bool) (Result, error) {
	p.init()
	p.final = true
	if _, err := p.Sync(ctx); err != nil {
		return Result{}, err
	}
	status := p.status()
	if status.Segments == 0 {
		return Result{}, errors.New("nothing was encoded: not one complete segment was written for every rung")
	}
	if clean && p.Window == 0 && !status.Ended {
		return Result{}, errors.New("ffmpeg exited without closing the playlists with ENDLIST")
	}

	result := Result{Segments: status.Segments, Playlists: map[string]pipeline.MediaPlaylist{}}
	rendered := p.Window > 0 || !status.Ended
	for _, rung := range p.Rungs {
		pl := p.last[rung].Truncate(status.Segments)
		pl.Ended = true
		result.Playlists[rung] = pl
		if rendered {
			if err := p.putPlaylist(ctx, path.Join(p.Prefix, rung, RungPlaylist), pl.Render(0, true)); err != nil {
				return Result{}, err
			}
		}
		if p.Window > 0 {
			// The stream is over; the index now is the whole stream, and a
			// second copy of the same list would only be something to keep in
			// step.
			_ = p.Store.Delete(ctx, path.Join(p.Prefix, rung, DVRPlaylist))
		}
	}
	if p.Window > 0 {
		_ = p.Store.Delete(ctx, path.Join(p.Prefix, DVRPlaylist))
	}
	result.DurationMs = result.Playlists[p.Rungs[0]].DurationMs()

	if !p.master {
		if err := p.syncMaster(ctx); err != nil {
			return Result{}, err
		}
		if !p.master {
			return Result{}, fmt.Errorf("ffmpeg wrote no usable %s", pipeline.MasterPlaylist)
		}
	} else if err := p.syncMaster(ctx); err != nil {
		// ffmpeg rewrites the master on exit with the bitrates it measured.
		return Result{}, err
	}
	if err := p.verify(ctx, result); err != nil {
		return Result{}, err
	}
	return result, nil
}

// verify is the last look before an asset is called ready: every file the
// final playlists name must be in the store, whole. Uploading as the encode
// went means a failed upload was already an error, so this should find
// nothing — it is here because "should" is the word in front of every asset
// that was marked ready with a hole in it.
func (p *Publisher) verify(ctx context.Context, result Result) error {
	for _, rung := range p.Rungs {
		for _, name := range result.Playlists[rung].Files() {
			key := path.Join(p.Prefix, rung, name)
			want, sent := p.sent[key]
			if !sent {
				return fmt.Errorf("%s is listed in the final playlist and was never uploaded", key)
			}
			if err := p.sized(ctx, key, want); err != nil {
				// One repair, from local disk if the file is still there.
				delete(p.sent, key)
				if perr := p.putFile(ctx, rung, name); perr != nil {
					return fmt.Errorf("%w (and it could not be uploaded again: %v)", err, perr)
				}
				if err := p.sized(ctx, key, p.sent[key]); err != nil {
					return err
				}
			}
		}
		if err := p.sized(ctx, path.Join(p.Prefix, rung, RungPlaylist), -1); err != nil {
			return err
		}
	}
	return p.sized(ctx, path.Join(p.Prefix, pipeline.MasterPlaylist), -1)
}

// sized checks a key is in the store, and at the expected size when one is
// given.
func (p *Publisher) sized(ctx context.Context, key string, want int64) error {
	body, size, err := p.Store.Open(ctx, key)
	if err != nil {
		return fmt.Errorf("%s is not in the store: %w", key, err)
	}
	body.Close()
	if want >= 0 && size != want {
		return fmt.Errorf("%s is %d bytes in the store and %d were uploaded", key, size, want)
	}
	return nil
}

// Frames uploads the still frames in dir under prefix/frames and returns their
// keys in time order. A directory that is not there is no frames, not an
// error: sampling is best-effort everywhere it is done.
func Frames(ctx context.Context, s store.Store, prefix, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	// ReadDir sorts by name, the names are zero-padded counters, and so name
	// order is the order the frames happened in.
	var keys []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jpg") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		obj, err := s.Put(ctx, path.Join(prefix, "frames", e.Name()), f)
		f.Close()
		if err != nil {
			return nil, err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}
