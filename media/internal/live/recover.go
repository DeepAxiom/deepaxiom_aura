package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"aura/media/internal/assets"
	"aura/media/internal/pipeline"
	"aura/media/internal/publish"
	"aura/media/internal/store"
)

// adoptOrphans closes streams whose node stopped renewing them.
//
// A node that is killed mid-stream cannot close the playlists it was writing.
// Without this, the asset would stay in processing for ever, growing, with a
// playlist that never ends — and a player treats that as a live stream that
// is buffering. Everything needed to close it is already in the store: every
// segment a playlist lists was uploaded before the playlist was.
func (m *Manager) adoptOrphans(ctx context.Context) {
	ids, err := m.cfg.Repo.Orphans(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.cfg.Log.Error("could not look for abandoned live streams", "error", err)
		}
		return
	}
	for _, streamID := range ids {
		m.mu.Lock()
		_, mine := m.sessions[streamID]
		m.mu.Unlock()
		if mine {
			// This node is encoding it and is merely late with a heartbeat —
			// a database that was unreachable for a while. Not an orphan.
			continue
		}
		s, err := m.cfg.Repo.Adopt(ctx, streamID, m.node, m.cfg.Lease)
		if err != nil {
			continue // another node adopted it first, or its own came back
		}
		log := m.cfg.Log.With("stream", s.ID, "asset", s.AssetID)
		detail := ""
		if err := CloseFromStore(ctx, m.cfg.Assets, m.cfg.Store, s.AssetID); err != nil {
			log.Error("an abandoned live stream could not be closed into a VOD", "error", err)
			detail = err.Error()
		} else {
			log.Warn("closed a live stream whose node stopped mid-broadcast into a VOD")
		}
		if err := m.cfg.Repo.Finish(ctx, s.ID, m.node, detail); err != nil {
			log.Error("could not record the end of the stream", "error", err)
		}
	}
}

// CloseFromStore turns what a live stream published into a finished VOD,
// using nothing but the store: the playlists there are read, cut back to the
// segments every rung has, closed with ENDLIST and written back.
//
// It is what runs when the node that was encoding is gone. The asset ends
// ready when there was anything to close, and failed when the stream died
// before its first segment was published.
func CloseFromStore(ctx context.Context, repo *assets.Repo, objects store.Store, assetID string) error {
	asset, err := repo.Get(ctx, assetID)
	if errors.Is(err, assets.ErrNotFound) {
		return nil // nothing was ever encoded; there is no asset to close
	}
	if err != nil {
		return err
	}
	if asset.State != assets.StateProcessing {
		return nil // already closed, one way or the other
	}
	fail := func(reason string) error {
		if err := repo.MarkFailed(ctx, assetID, reason); err != nil {
			return err
		}
		return errors.New(reason)
	}
	if asset.Address == "" || len(asset.Outputs.Renditions) == 0 {
		return fail("the node encoding this stream stopped before anything was published")
	}

	playlists := make([]pipeline.MediaPlaylist, len(asset.Outputs.Renditions))
	shortest := -1
	for i, r := range asset.Outputs.Renditions {
		// The whole-stream playlist when the live one was a window; otherwise
		// the live playlist is the whole stream.
		dvr := strings.TrimSuffix(r.Playlist, publish.RungPlaylist) + publish.DVRPlaylist
		raw, err := readObject(ctx, objects, dvr)
		if errors.Is(err, store.ErrNotFound) {
			raw, err = readObject(ctx, objects, r.Playlist)
		}
		if err != nil {
			return fail(fmt.Sprintf("the %s playlist of this stream could not be read back: %v", r.Name, err))
		}
		pl, err := pipeline.ParseMediaPlaylist(raw)
		if err != nil {
			return fail(fmt.Sprintf("the %s playlist of this stream is unreadable: %v", r.Name, err))
		}
		playlists[i] = pl
		if shortest < 0 || len(pl.Segments) < shortest {
			shortest = len(pl.Segments)
		}
	}
	if shortest <= 0 {
		return fail("the node encoding this stream stopped before a segment was published")
	}

	out := asset.Outputs
	for i, r := range asset.Outputs.Renditions {
		pl := playlists[i].Truncate(shortest)
		if _, err := objects.Put(ctx, r.Playlist, strings.NewReader(string(pl.Render(0, true)))); err != nil {
			return err
		}
		dir := path.Dir(r.Playlist)
		_ = objects.Delete(ctx, path.Join(dir, publish.DVRPlaylist))
		if i == 0 {
			out.DurationMs = pl.DurationMs()
			_ = objects.Delete(ctx, path.Join(path.Dir(dir), publish.DVRPlaylist))
		}
	}
	return repo.MarkReady(ctx, assetID, asset.Address, out)
}

func readObject(ctx context.Context, objects store.Store, key string) ([]byte, error) {
	body, _, err := objects.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 32<<20))
}
