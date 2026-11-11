// Package assets is the record of what came in and what came out.
//
// One row per asset, and it is the only thing a consumer needs to read: an
// asset in, an address out. The jobs behind it are this subsystem's business.
package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State of an asset, as a consumer sees it.
type State string

const (
	// StateReceived means the bytes are stored and nothing has run yet.
	StateReceived State = "received"
	// StateProcessing means a worker holds a job for it right now. Since 0.2.0
	// an asset in this state may already have an Address: the package is
	// published as it is written, and Growing says so.
	StateProcessing State = "processing"
	// StateReady means Address is fetchable and the package behind it is
	// complete. That second half is the promise existing consumers poll for,
	// and publishing early did not change it.
	StateReady State = "ready"
	// StateFailed means every attempt was spent. Error says what happened.
	StateFailed State = "failed"
)

// Kind of asset: where its bytes came from.
const (
	KindVOD  = "vod"
	KindLive = "live"
)

// Rendition is one video variant in the output ladder.
type Rendition struct {
	Name        string `json:"name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	BitrateKbps int    `json:"bitrate_kbps"`
	Playlist    string `json:"playlist"`
}

// Outputs is everything a finished asset produced. Frames are here from the
// first version on purpose: every AI capability this subsystem exists to serve
// needs frames, and extracting them during the encode costs one more output on
// a decode that is already running.
type Outputs struct {
	Renditions []Rendition `json:"renditions,omitempty"`
	Frames     []string    `json:"frames,omitempty"`
	Poster     string      `json:"poster,omitempty"`
	DurationMs int64       `json:"duration_ms,omitempty"`
}

// Asset is one media object and what became of it.
type Asset struct {
	ID           string  `json:"id"`
	State        State   `json:"state"`
	Kind         string  `json:"kind"`
	SourceKey    string  `json:"-"`
	SourceBytes  int64   `json:"source_bytes"`
	SourceSHA256 string  `json:"source_sha256"`
	MIME         string  `json:"mime,omitempty"`
	DurationMs   int64   `json:"duration_ms,omitempty"`
	Address      string  `json:"address,omitempty"`
	Outputs      Outputs `json:"outputs"`
	Error        string  `json:"error,omitempty"`
	// Progress is how far through the encode this asset is, 0..1. It is 1 for
	// every ready asset, including the ones encoded before the column existed.
	// A live asset has no length to be a fraction of and stays at 0 until the
	// stream ends.
	Progress float64 `json:"progress"`
	// Growing is true while Address is playable but the package is still being
	// added to. A consumer that only wants finished assets ignores it and
	// waits for State to be ready, exactly as before.
	Growing   bool      `json:"growing,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrNotFound is returned for an id that is not in the table.
var ErrNotFound = errors.New("no such asset")

// Repo reads and writes assets.
type Repo struct{ pool *pgxpool.Pool }

// Open wraps a pool. The pool's lifetime belongs to the caller.
func Open(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const columns = `id, state, source_key, source_bytes, source_sha256, mime,
	duration_ms, address, outputs, error, created_at, updated_at,
	progress, growing, kind`

func scan(row pgx.Row) (Asset, error) {
	var a Asset
	var outputs []byte
	err := row.Scan(&a.ID, &a.State, &a.SourceKey, &a.SourceBytes, &a.SourceSHA256,
		&a.MIME, &a.DurationMs, &a.Address, &outputs, &a.Error, &a.CreatedAt, &a.UpdatedAt,
		&a.Progress, &a.Growing, &a.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, err
	}
	if len(outputs) > 0 {
		if err := json.Unmarshal(outputs, &a.Outputs); err != nil {
			return Asset{}, fmt.Errorf("asset %s has unreadable outputs: %w", a.ID, err)
		}
	}
	// A row written before 0.2.0 has the column's default, 0, whatever state
	// it reached. Ready has always meant complete, so it reads as complete
	// here rather than by rewriting every old row during an upgrade.
	if a.State == StateReady {
		a.Progress, a.Growing = 1, false
	}
	return a, nil
}

// Create records a stored source. The bytes are already in the object store by
// the time this runs: a row for an asset whose bytes are not there yet is a row
// a worker can claim and fail on.
func (r *Repo) Create(ctx context.Context, a Asset) (Asset, error) {
	if a.ID == "" || a.SourceKey == "" {
		return Asset{}, errors.New("an asset needs an id and a stored source")
	}
	const sql = `INSERT INTO media_asset (id, state, source_key, source_bytes, source_sha256, mime)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + columns
	created, err := scan(r.pool.QueryRow(ctx, sql, a.ID, StateReceived, a.SourceKey,
		a.SourceBytes, a.SourceSHA256, a.MIME))
	if err != nil {
		return Asset{}, fmt.Errorf("create asset: %w", err)
	}
	return created, nil
}

// CreateLive records an asset whose source is a stream that has just started
// arriving. It is born in processing: there is no stored source for a worker
// to pick up, and the encode is already running.
func (r *Repo) CreateLive(ctx context.Context, id string) (Asset, error) {
	if id == "" {
		return Asset{}, errors.New("an asset needs an id")
	}
	const sql = `INSERT INTO media_asset (id, state, source_key, kind)
		VALUES ($1, 'processing', '', 'live')
		RETURNING ` + columns
	created, err := scan(r.pool.QueryRow(ctx, sql, id))
	if err != nil {
		return Asset{}, fmt.Errorf("create live asset: %w", err)
	}
	return created, nil
}

// Get reads one asset.
func (r *Repo) Get(ctx context.Context, id string) (Asset, error) {
	const sql = `SELECT ` + columns + ` FROM media_asset WHERE id = $1`
	return scan(r.pool.QueryRow(ctx, sql, id))
}

// BySourceDigest finds an asset already ingested with this digest, so the same
// bytes uploaded twice do not get encoded twice. Returns ErrNotFound when there
// is none.
func (r *Repo) BySourceDigest(ctx context.Context, sha256 string) (Asset, error) {
	if sha256 == "" {
		return Asset{}, ErrNotFound
	}
	const sql = `SELECT ` + columns + ` FROM media_asset
		WHERE source_sha256 = $1 AND state <> 'failed'
		ORDER BY created_at LIMIT 1`
	return scan(r.pool.QueryRow(ctx, sql, sha256))
}

// List returns assets newest first.
func (r *Repo) List(ctx context.Context, limit int) ([]Asset, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const sql = `SELECT ` + columns + ` FROM media_asset ORDER BY created_at DESC LIMIT $1`
	rows, err := r.pool.Query(ctx, sql, limit)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Delete removes an asset row. Used for one thing only: an asset this node
// created and then found it should not have, in the race where two requests
// carrying the same idempotency key both got as far as storing bytes. Leaving
// it would be leaving a row in "received" that nothing will ever encode.
func (r *Repo) Delete(ctx context.Context, id string) error {
	const sql = `DELETE FROM media_asset WHERE id = $1`
	return r.exec(ctx, sql, id)
}

// MarkProcessing says a worker is on it. It also takes back anything an
// earlier attempt published: an address that points at a package nobody is
// writing any more is worse than no address.
func (r *Repo) MarkProcessing(ctx context.Context, id string) error {
	const sql = `UPDATE media_asset SET
			state = 'processing', address = '', growing = false, progress = 0,
			updated_at = now()
		WHERE id = $1`
	return r.exec(ctx, sql, id)
}

// MarkPublished hands out the address while the package is still being
// written. The state does not move: it stays processing, and Growing is what
// tells a consumer the address can already be played.
//
// Only an asset that is being processed can be published this way. A worker
// that lost its lease and is slow to notice must not put an address back on an
// asset another worker has since failed or finished.
func (r *Repo) MarkPublished(ctx context.Context, id, address string, out Outputs) error {
	encoded, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("asset outputs: %w", err)
	}
	const sql = `UPDATE media_asset SET
			address = $2, outputs = $3, duration_ms = $4, growing = true,
			error = '', updated_at = now()
		WHERE id = $1 AND state = 'processing'`
	return r.exec(ctx, sql, id, address, encoded, out.DurationMs)
}

// Unpublish takes an early address back. Used when the encode that was
// publishing as it went has failed and another way of encoding the same asset
// is about to be tried: what was published belongs to the attempt that died.
func (r *Repo) Unpublish(ctx context.Context, id string) error {
	const sql = `UPDATE media_asset SET
			address = '', growing = false, progress = 0, outputs = '{}'::jsonb,
			updated_at = now()
		WHERE id = $1 AND state = 'processing'`
	return r.exec(ctx, sql, id)
}

// SetProgress records how far the encode has got. It never moves backwards
// within an attempt, and it never touches an asset that is not being
// processed. Not finding such an asset is not an error worth a job: the
// progress of work that has already ended is simply not interesting.
func (r *Repo) SetProgress(ctx context.Context, id string, fraction float64) error {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	const sql = `UPDATE media_asset SET progress = greatest(progress, $2), updated_at = now()
		WHERE id = $1 AND state = 'processing'`
	_, err := r.pool.Exec(ctx, sql, id, fraction)
	return err
}

// MarkReady publishes the address and everything the encode produced. This is
// the only transition a consumer is waiting for.
func (r *Repo) MarkReady(ctx context.Context, id, address string, out Outputs) error {
	encoded, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("asset outputs: %w", err)
	}
	const sql = `UPDATE media_asset SET
			state = 'ready', address = $2, outputs = $3, duration_ms = $4,
			error = '', growing = false, progress = 1, updated_at = now()
		WHERE id = $1`
	return r.exec(ctx, sql, id, address, encoded, out.DurationMs)
}

// MarkFailed records a terminal failure in the words the operator will read.
// Any early address goes with it: a failed asset has never had one, and a
// player left pointing at half a package would wait on it for ever.
func (r *Repo) MarkFailed(ctx context.Context, id, detail string) error {
	const sql = `UPDATE media_asset SET
			state = 'failed', error = $2, address = '', growing = false,
			updated_at = now()
		WHERE id = $1`
	return r.exec(ctx, sql, id, detail)
}

// Requeued puts an asset back to received after a retryable failure, keeping
// the reason visible. The asset is not failed — something will run again — but
// a state that still said "processing" with nobody processing would be a lie an
// operator has no way to see through.
func (r *Repo) Requeued(ctx context.Context, id, detail string) error {
	const sql = `UPDATE media_asset SET
			state = 'received', error = $2, address = '', growing = false,
			updated_at = now()
		WHERE id = $1`
	return r.exec(ctx, sql, id, detail)
}

func (r *Repo) exec(ctx context.Context, sql string, args ...any) error {
	tag, err := r.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
