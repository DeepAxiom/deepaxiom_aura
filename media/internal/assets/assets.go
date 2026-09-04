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
	// StateProcessing means a worker holds a job for it right now.
	StateProcessing State = "processing"
	// StateReady means Address is fetchable.
	StateReady State = "ready"
	// StateFailed means every attempt was spent. Error says what happened.
	StateFailed State = "failed"
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
	ID           string    `json:"id"`
	State        State     `json:"state"`
	SourceKey    string    `json:"-"`
	SourceBytes  int64     `json:"source_bytes"`
	SourceSHA256 string    `json:"source_sha256"`
	MIME         string    `json:"mime,omitempty"`
	DurationMs   int64     `json:"duration_ms,omitempty"`
	Address      string    `json:"address,omitempty"`
	Outputs      Outputs   `json:"outputs"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ErrNotFound is returned for an id that is not in the table.
var ErrNotFound = errors.New("no such asset")

// Repo reads and writes assets.
type Repo struct{ pool *pgxpool.Pool }

// Open wraps a pool. The pool's lifetime belongs to the caller.
func Open(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

const columns = `id, state, source_key, source_bytes, source_sha256, mime,
	duration_ms, address, outputs, error, created_at, updated_at`

func scan(row pgx.Row) (Asset, error) {
	var a Asset
	var outputs []byte
	err := row.Scan(&a.ID, &a.State, &a.SourceKey, &a.SourceBytes, &a.SourceSHA256,
		&a.MIME, &a.DurationMs, &a.Address, &outputs, &a.Error, &a.CreatedAt, &a.UpdatedAt)
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

// MarkProcessing says a worker is on it.
func (r *Repo) MarkProcessing(ctx context.Context, id string) error {
	const sql = `UPDATE media_asset SET state = 'processing', updated_at = now() WHERE id = $1`
	return r.exec(ctx, sql, id)
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
			error = '', updated_at = now()
		WHERE id = $1`
	return r.exec(ctx, sql, id, address, encoded, out.DurationMs)
}

// MarkFailed records a terminal failure in the words the operator will read.
func (r *Repo) MarkFailed(ctx context.Context, id, detail string) error {
	const sql = `UPDATE media_asset SET state = 'failed', error = $2, updated_at = now() WHERE id = $1`
	return r.exec(ctx, sql, id, detail)
}

// Requeued puts an asset back to received after a retryable failure, keeping
// the reason visible. The asset is not failed — something will run again — but
// a state that still said "processing" with nobody processing would be a lie an
// operator has no way to see through.
func (r *Repo) Requeued(ctx context.Context, id, detail string) error {
	const sql = `UPDATE media_asset SET state = 'received', error = $2, updated_at = now() WHERE id = $1`
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
