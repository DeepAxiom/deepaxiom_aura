// Package queue is the work queue: rows in Postgres, claimed by N workers with
// FOR UPDATE SKIP LOCKED.
//
// Why a database and not a channel in the process: an encode is minutes long,
// so a worker that dies mid-job must not take the job with it. A claim is a
// lease with a deadline; a worker that stops renewing loses the job to whoever
// asks next. That is also why the parallelism is simply the worker count —
// SKIP LOCKED hands each caller a different row without any of them waiting on
// the others.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State of a job. A job is claimable only in StateQueued.
type State string

const (
	StateQueued  State = "queued"
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Kind of work. One per pipeline stage, so a re-encode does not have to redo
// the ingest and a frame extraction can be scheduled on its own.
const (
	KindTranscode = "transcode"
)

// Job is one unit of work.
type Job struct {
	ID          int64
	AssetID     string
	Kind        string
	State       State
	Priority    int
	Attempts    int
	MaxAttempts int
	Worker      string
	LeaseUntil  time.Time
	RunAfter    time.Time
	LastError   string
	Idem        string
	Args        map[string]any
	CreatedAt   time.Time
}

// New describes a job to enqueue.
type New struct {
	AssetID     string
	Kind        string
	Priority    int
	MaxAttempts int
	// Idem makes enqueueing idempotent. Empty means "no deduplication", which
	// is right for an operator-triggered retry and wrong for anything a caller
	// may deliver twice.
	Idem string
	Args map[string]any
}

// ErrNoJob is what Claim returns when the queue has nothing claimable. It is
// not a failure: it is the normal answer on an idle node, so a worker treats
// it as "sleep and ask again", never as an error to report.
var ErrNoJob = errors.New("no claimable job")

// Queue is the queue over an existing pool.
type Queue struct{ pool *pgxpool.Pool }

// Open wraps a pool. The pool's lifetime belongs to the caller.
func Open(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }

const jobColumns = `id, asset_id, kind, state, priority, attempts, max_attempts,
	worker, coalesce(lease_until, 'epoch'::timestamptz), run_after, last_error,
	coalesce(idem, ''), args, created_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var args []byte
	err := row.Scan(&j.ID, &j.AssetID, &j.Kind, &j.State, &j.Priority, &j.Attempts,
		&j.MaxAttempts, &j.Worker, &j.LeaseUntil, &j.RunAfter, &j.LastError,
		&j.Idem, &args, &j.CreatedAt)
	if err != nil {
		return Job{}, err
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &j.Args); err != nil {
			return Job{}, fmt.Errorf("job %d has unreadable args: %w", j.ID, err)
		}
	}
	return j, nil
}

// Enqueue adds a job, or returns the existing one when Idem has been seen
// before. Returning the existing job rather than an error is what lets an
// at-least-once caller — the HTTP API with an Idempotency-Key, a redelivered
// C3 envelope — simply enqueue again and read the state of the work already
// under way.
func (q *Queue) Enqueue(ctx context.Context, n New) (Job, error) {
	if n.AssetID == "" || n.Kind == "" {
		return Job{}, errors.New("a job needs an asset and a kind")
	}
	if n.MaxAttempts <= 0 {
		n.MaxAttempts = 3
	}
	if n.Priority == 0 {
		n.Priority = 100
	}
	args := []byte("{}")
	if n.Args != nil {
		encoded, err := json.Marshal(n.Args)
		if err != nil {
			return Job{}, fmt.Errorf("job args: %w", err)
		}
		args = encoded
	}
	var idem any
	if n.Idem != "" {
		idem = n.Idem
	}
	const insert = `INSERT INTO media_job (asset_id, kind, priority, max_attempts, idem, args)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idem) DO NOTHING
		RETURNING ` + jobColumns
	job, err := scanJob(q.pool.QueryRow(ctx, insert, n.AssetID, n.Kind, n.Priority, n.MaxAttempts, idem, args))
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || n.Idem == "" {
		return Job{}, fmt.Errorf("enqueue: %w", err)
	}
	// DO NOTHING fired: this idempotency key is already in the table.
	const existing = `SELECT ` + jobColumns + ` FROM media_job WHERE idem = $1`
	job, err = scanJob(q.pool.QueryRow(ctx, existing, n.Idem))
	if err != nil {
		return Job{}, fmt.Errorf("enqueue found a duplicate it cannot read: %w", err)
	}
	return job, nil
}

// Claim takes the next claimable job and leases it to worker for lease.
// Returns ErrNoJob when there is nothing to do.
//
// SKIP LOCKED is the whole point: two workers running this statement at the
// same instant get two different rows, and neither waits for the other. Without
// it the second blocks on the first's row lock and the queue serialises.
func (q *Queue) Claim(ctx context.Context, worker string, lease time.Duration) (Job, error) {
	if worker == "" {
		return Job{}, errors.New("a claim must name its worker")
	}
	const claim = `UPDATE media_job SET
			state       = 'running',
			worker      = $1,
			attempts    = attempts + 1,
			lease_until = now() + make_interval(secs => $2),
			updated_at  = now()
		WHERE id = (
			SELECT id FROM media_job
			 WHERE state = 'queued' AND run_after <= now()
			 ORDER BY priority, run_after, id
			 FOR UPDATE SKIP LOCKED
			 LIMIT 1
		)
		RETURNING ` + jobColumns
	job, err := scanJob(q.pool.QueryRow(ctx, claim, worker, lease.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNoJob
	}
	if err != nil {
		return Job{}, fmt.Errorf("claim: %w", err)
	}
	return job, nil
}

// Heartbeat extends a lease. A worker that cannot renew — because the row was
// reclaimed under it — is told so, and must stop: something else owns the job.
func (q *Queue) Heartbeat(ctx context.Context, id int64, worker string, lease time.Duration) error {
	const sql = `UPDATE media_job SET lease_until = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = $1 AND worker = $2 AND state = 'running'`
	tag, err := q.pool.Exec(ctx, sql, id, worker, lease.Seconds())
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("job %d is no longer leased to %s", id, worker)
	}
	return nil
}

// Complete marks a job done.
func (q *Queue) Complete(ctx context.Context, id int64, worker string) error {
	const sql = `UPDATE media_job SET state = 'done', lease_until = NULL, last_error = '', updated_at = now()
		WHERE id = $1 AND worker = $2 AND state = 'running'`
	tag, err := q.pool.Exec(ctx, sql, id, worker)
	if err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("job %d is not running under %s", id, worker)
	}
	return nil
}

// Fail records a failure: back to the queue after a backoff while attempts
// remain, terminal otherwise. Returns whether the job will be retried, because
// the caller's log line differs in each case and a terminal failure that reads
// like a retry is how a queue loses work quietly.
func (q *Queue) Fail(ctx context.Context, id int64, worker string, cause error) (bool, error) {
	detail := "unknown failure"
	if cause != nil {
		detail = cause.Error()
	}
	const sql = `UPDATE media_job SET
			state       = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
			run_after   = now() + make_interval(secs => $3),
			lease_until = NULL,
			last_error  = $4,
			updated_at  = now()
		WHERE id = $1 AND worker = $2 AND state = 'running'
		RETURNING state`
	var state State
	// The backoff is computed here rather than in SQL: it is a policy, it is
	// worth a unit test, and `attempts` in that statement is already the
	// post-claim value, which is exactly the kind of off-by-one that writes a
	// retry storm by accident.
	var attempts int
	if err := q.pool.QueryRow(ctx, `SELECT attempts FROM media_job WHERE id = $1`, id).Scan(&attempts); err != nil {
		return false, fmt.Errorf("job %d: %w", id, err)
	}
	row := q.pool.QueryRow(ctx, sql, id, worker, Backoff(attempts).Seconds(), detail)
	if err := row.Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("job %d is not running under %s", id, worker)
		}
		return false, fmt.Errorf("fail: %w", err)
	}
	return state == StateQueued, nil
}

// Reclaim returns jobs whose lease expired to the queue, and gives up on the
// ones with no attempts left. Returns how many rows it moved.
//
// This is what makes a killed worker survivable: the job is not lost with the
// process, it simply stops being renewed, and the next reclaim puts it back.
func (q *Queue) Reclaim(ctx context.Context) (int, error) {
	const sql = `UPDATE media_job SET
			state       = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
			worker      = '',
			lease_until = NULL,
			last_error  = CASE WHEN last_error = '' THEN 'lease expired: the worker stopped renewing it' ELSE last_error END,
			updated_at  = now()
		WHERE state = 'running' AND lease_until IS NOT NULL AND lease_until < now()`
	tag, err := q.pool.Exec(ctx, sql)
	if err != nil {
		return 0, fmt.Errorf("reclaim: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Get reads one job.
func (q *Queue) Get(ctx context.Context, id int64) (Job, error) {
	const sql = `SELECT ` + jobColumns + ` FROM media_job WHERE id = $1`
	job, err := scanJob(q.pool.QueryRow(ctx, sql, id))
	if err != nil {
		return Job{}, fmt.Errorf("job %d: %w", id, err)
	}
	return job, nil
}

// ForAsset lists an asset's jobs, newest first.
func (q *Queue) ForAsset(ctx context.Context, assetID string) ([]Job, error) {
	const sql = `SELECT ` + jobColumns + ` FROM media_job WHERE asset_id = $1 ORDER BY id DESC`
	rows, err := q.pool.Query(ctx, sql, assetID)
	if err != nil {
		return nil, fmt.Errorf("jobs of %s: %w", assetID, err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// Depth counts what is waiting and what is running: the two numbers behind
// "is it stuck or is it busy".
func (q *Queue) Depth(ctx context.Context) (queued, running int, err error) {
	const sql = `SELECT
			count(*) FILTER (WHERE state = 'queued'),
			count(*) FILTER (WHERE state = 'running')
		FROM media_job`
	if err := q.pool.QueryRow(ctx, sql).Scan(&queued, &running); err != nil {
		return 0, 0, fmt.Errorf("depth: %w", err)
	}
	return queued, running, nil
}

// Backoff is how long to wait before retrying after a given attempt count.
// Exponential and capped: a source file ffmpeg cannot decode fails in the same
// second every time, and three of those in a tight loop is a worker doing
// nothing else.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(1<<min(attempt-1, 6)) * 10 * time.Second
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}
