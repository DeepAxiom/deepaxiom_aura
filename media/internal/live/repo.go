package live

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State of a live stream.
type State string

const (
	// StateIdle means the stream exists and nobody has published to it yet.
	StateIdle State = "idle"
	// StateLive means a node is encoding it: a broadcaster is connected, or
	// was a moment ago and is still inside the reconnect window.
	StateLive State = "live"
	// StateEnding means someone asked through the API for the stream to stop,
	// and the node encoding it has not yet noticed.
	StateEnding State = "ending"
	// StateEnded means it is over. If anything was broadcast, the asset is a
	// VOD now; the key is dead either way.
	StateEnded State = "ended"
)

// Stream is one live stream.
type Stream struct {
	ID      string `json:"id"`
	State   State  `json:"state"`
	AssetID string `json:"asset_id"`
	// KeyHash never leaves the process.
	KeyHash   string     `json:"-"`
	Node      string     `json:"-"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// ErrNotFound is returned for a stream id that is not in the table.
var ErrNotFound = errors.New("no such live stream")

// ErrNotHeld is returned when a node tries to act on a stream it does not, or
// no longer, hold: it was never idle, it was ended through the API, or another
// node adopted it.
var ErrNotHeld = errors.New("the stream is not held by this node")

// Repo reads and writes live streams.
type Repo struct{ pool *pgxpool.Pool }

// OpenRepo wraps a pool. The pool's lifetime belongs to the caller.
func OpenRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const columns = `id, state, key_hash, asset_id, node, error, created_at, started_at, ended_at`

func scan(row pgx.Row) (Stream, error) {
	var s Stream
	err := row.Scan(&s.ID, &s.State, &s.KeyHash, &s.AssetID, &s.Node, &s.Error,
		&s.CreatedAt, &s.StartedAt, &s.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stream{}, ErrNotFound
	}
	return s, err
}

// Create records a new stream, idle.
func (r *Repo) Create(ctx context.Context, id, keyHash, assetID string) (Stream, error) {
	if id == "" || keyHash == "" || assetID == "" {
		return Stream{}, errors.New("a live stream needs an id, a key hash and an asset id")
	}
	const sql = `INSERT INTO media_live (id, key_hash, asset_id) VALUES ($1, $2, $3) RETURNING ` + columns
	s, err := scan(r.pool.QueryRow(ctx, sql, id, keyHash, assetID))
	if err != nil {
		return Stream{}, fmt.Errorf("create live stream: %w", err)
	}
	return s, nil
}

// Get reads one stream.
func (r *Repo) Get(ctx context.Context, id string) (Stream, error) {
	const sql = `SELECT ` + columns + ` FROM media_live WHERE id = $1`
	return scan(r.pool.QueryRow(ctx, sql, id))
}

// List returns streams newest first.
func (r *Repo) List(ctx context.Context, limit int) ([]Stream, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const sql = `SELECT ` + columns + ` FROM media_live ORDER BY created_at DESC LIMIT $1`
	rows, err := r.pool.Query(ctx, sql, limit)
	if err != nil {
		return nil, fmt.Errorf("list live streams: %w", err)
	}
	defer rows.Close()
	out := []Stream{}
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Claim takes an idle stream for a node. Exactly one caller wins: the row is
// the lock, so two broadcasters presenting the same key to two nodes at the
// same instant cannot both be accepted.
func (r *Repo) Claim(ctx context.Context, id, node string, lease time.Duration) (Stream, error) {
	const sql = `UPDATE media_live SET
			state = 'live', node = $2, error = '',
			lease_until = now() + make_interval(secs => $3),
			started_at = coalesce(started_at, now()), updated_at = now()
		WHERE id = $1 AND state = 'idle'
		RETURNING ` + columns
	s, err := scan(r.pool.QueryRow(ctx, sql, id, node, lease.Seconds()))
	if errors.Is(err, ErrNotFound) {
		return Stream{}, ErrNotHeld
	}
	return s, err
}

// Heartbeat renews a node's hold. ErrNotHeld means stop: the stream was ended
// through the API, or another node decided this one was dead and adopted it.
func (r *Repo) Heartbeat(ctx context.Context, id, node string, lease time.Duration) error {
	const sql = `UPDATE media_live SET lease_until = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = $1 AND node = $2 AND state = 'live'`
	tag, err := r.pool.Exec(ctx, sql, id, node, lease.Seconds())
	if err != nil {
		return fmt.Errorf("live heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotHeld
	}
	return nil
}

// Release puts a stream back to idle. Used when a broadcaster connected and
// nothing usable arrived — no video, or nothing ffprobe could read — so the
// key is still good for a second try with the encoder set up properly.
func (r *Repo) Release(ctx context.Context, id, node, reason string) error {
	const sql = `UPDATE media_live SET
			state = 'idle', node = '', lease_until = NULL, error = $3, updated_at = now()
		WHERE id = $1 AND node = $2 AND state = 'live'`
	_, err := r.pool.Exec(ctx, sql, id, node, reason)
	return err
}

// Finish marks a stream over. Only the node holding it may, which is what
// stops a node that lost the stream from closing it under the node that has
// it now.
func (r *Repo) Finish(ctx context.Context, id, node, reason string) error {
	const sql = `UPDATE media_live SET
			state = 'ended', lease_until = NULL, error = $3,
			ended_at = now(), updated_at = now()
		WHERE id = $1 AND node = $2 AND state IN ('live', 'ending')`
	tag, err := r.pool.Exec(ctx, sql, id, node, reason)
	if err != nil {
		return fmt.Errorf("finish live stream: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotHeld
	}
	return nil
}

// RequestEnd is the API's way to stop a stream. An idle one is ended on the
// spot, which revokes its key. A live one is marked ending: the node encoding
// it finds out at its next heartbeat, which fails, and closes the stream the
// same way it would if the broadcaster had stopped.
func (r *Repo) RequestEnd(ctx context.Context, id string) (Stream, error) {
	const sql = `UPDATE media_live SET
			state = CASE WHEN state = 'idle' THEN 'ended' ELSE 'ending' END,
			ended_at = CASE WHEN state = 'idle' THEN now() ELSE ended_at END,
			updated_at = now()
		WHERE id = $1 AND state IN ('idle', 'live')
		RETURNING ` + columns
	s, err := scan(r.pool.QueryRow(ctx, sql, id))
	if errors.Is(err, ErrNotFound) {
		// Already ending or ended, or never existed: Get tells them apart.
		return r.Get(ctx, id)
	}
	return s, err
}

// Adopt takes over a stream whose node stopped renewing it, so that what it
// had published can be closed into a VOD. It returns ErrNotHeld when there is
// nothing to adopt — the node is alive after all, or another node got there
// first.
func (r *Repo) Adopt(ctx context.Context, id, node string, lease time.Duration) (Stream, error) {
	const sql = `UPDATE media_live SET
			node = $2, lease_until = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = $1 AND state IN ('live', 'ending') AND lease_until IS NOT NULL AND lease_until < now()
		RETURNING ` + columns
	s, err := scan(r.pool.QueryRow(ctx, sql, id, node, lease.Seconds()))
	if errors.Is(err, ErrNotFound) {
		return Stream{}, ErrNotHeld
	}
	return s, err
}

// Orphans lists streams whose node has stopped renewing them.
func (r *Repo) Orphans(ctx context.Context) ([]string, error) {
	const sql = `SELECT id FROM media_live
		WHERE state IN ('live', 'ending') AND lease_until IS NOT NULL AND lease_until < now()
		ORDER BY lease_until LIMIT 50`
	rows, err := r.pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("orphaned live streams: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
