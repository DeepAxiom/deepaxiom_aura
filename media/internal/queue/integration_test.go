package queue_test

// The claim is a concurrency claim, so it is tested against a real Postgres —
// SKIP LOCKED is exactly the kind of thing a mock would agree with and a server
// would not. Set AURA_MEDIA_TEST_DSN to run these; without it they skip, so
// `go test ./...` stays runnable on a machine with no database.
//
//	docker run --rm -e POSTGRES_PASSWORD=media -p 5433:5432 postgres:16
//	AURA_MEDIA_TEST_DSN=postgres://postgres:media@localhost:5433/postgres go test ./...
//
// Every test runs in a schema of its own — see internal/testdb.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"aura/media/internal/id"
	"aura/media/internal/queue"
	"aura/media/internal/testdb"
)

// Each test gets its own schema. These used to share one, and `go test ./...`
// runs packages in parallel: the API's tests truncated these tables mid-test,
// which showed up as a foreign key violation and a reclaim that found nothing.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testdb.Pool(t)
}

func newAsset(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	assetID := id.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO media_asset (id, state, source_key) VALUES ($1, 'received', $2)`,
		assetID, "src/"+assetID)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	return assetID
}

// The load-bearing one: N workers claiming at the same instant must divide the
// queue, not fight over the head of it.
func TestClaimHandsEachJobToExactlyOneWorker(t *testing.T) {
	pool := testPool(t)
	q := queue.Open(pool)
	ctx := context.Background()

	const jobs = 40
	const workers = 8
	for i := 0; i < jobs; i++ {
		if _, err := q.Enqueue(ctx, queue.New{AssetID: newAsset(t, pool), Kind: queue.KindTranscode}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	var mu sync.Mutex
	claimedBy := map[int64]string{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-start
			for {
				job, err := q.Claim(ctx, name, time.Minute)
				if errors.Is(err, queue.ErrNoJob) {
					return
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				if other, dup := claimedBy[job.ID]; dup {
					t.Errorf("job %d claimed twice: by %s and by %s", job.ID, other, name)
				}
				claimedBy[job.ID] = name
				mu.Unlock()
			}
		}(id.New())
	}
	close(start)
	wg.Wait()

	if len(claimedBy) != jobs {
		t.Fatalf("claimed %d jobs, enqueued %d", len(claimedBy), jobs)
	}
	queued, running, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if queued != 0 || running != jobs {
		t.Fatalf("depth is queued=%d running=%d, want 0 and %d", queued, running, jobs)
	}
}

func TestEnqueueIsIdempotentByKey(t *testing.T) {
	pool := testPool(t)
	q := queue.Open(pool)
	ctx := context.Background()
	assetID := newAsset(t, pool)

	first, err := q.Enqueue(ctx, queue.New{AssetID: assetID, Kind: queue.KindTranscode, Idem: "sess-1:asset_in:7"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	second, err := q.Enqueue(ctx, queue.New{AssetID: assetID, Kind: queue.KindTranscode, Idem: "sess-1:asset_in:7"})
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("a redelivered enqueue made a second job: %d then %d", first.ID, second.ID)
	}
	queued, _, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if queued != 1 {
		t.Fatalf("%d jobs queued, want 1", queued)
	}
}

// A worker that dies holds its job until the lease runs out, and no longer.
func TestReclaimTakesBackAnExpiredLease(t *testing.T) {
	pool := testPool(t)
	q := queue.Open(pool)
	ctx := context.Background()
	assetID := newAsset(t, pool)

	if _, err := q.Enqueue(ctx, queue.New{AssetID: assetID, Kind: queue.KindTranscode}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dead := "worker-that-dies"
	job, err := q.Claim(ctx, dead, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Still leased: a reclaim now must take nothing.
	if moved, err := q.Reclaim(ctx); err != nil || moved != 0 {
		t.Fatalf("reclaim took %d jobs before the lease expired (err %v)", moved, err)
	}
	time.Sleep(400 * time.Millisecond)
	moved, err := q.Reclaim(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if moved != 1 {
		t.Fatalf("reclaim moved %d jobs, want 1", moved)
	}

	// The dead worker must not be able to finish work it no longer owns.
	if err := q.Heartbeat(ctx, job.ID, dead, time.Minute); err == nil {
		t.Fatal("a reclaimed job still accepted a heartbeat from its old worker")
	}
	if err := q.Complete(ctx, job.ID, dead); err == nil {
		t.Fatal("a reclaimed job still accepted a completion from its old worker")
	}

	again, err := q.Claim(ctx, "worker-that-lives", time.Minute)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if again.ID != job.ID {
		t.Fatalf("re-claimed job %d, want the reclaimed %d", again.ID, job.ID)
	}
	if again.Attempts != 2 {
		t.Fatalf("attempts is %d after two claims, want 2", again.Attempts)
	}
}

func TestFailRetriesUntilAttemptsRunOut(t *testing.T) {
	pool := testPool(t)
	q := queue.Open(pool)
	ctx := context.Background()
	assetID := newAsset(t, pool)

	if _, err := q.Enqueue(ctx, queue.New{AssetID: assetID, Kind: queue.KindTranscode, MaxAttempts: 2}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	worker := "w1"
	job, err := q.Claim(ctx, worker, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	retrying, err := q.Fail(ctx, job.ID, worker, errors.New("ffmpeg exited 1"))
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if !retrying {
		t.Fatal("the first failure of a two-attempt job was terminal")
	}
	after, err := q.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.State != queue.StateQueued {
		t.Fatalf("state is %q after a retryable failure, want queued", after.State)
	}
	if !after.RunAfter.After(time.Now()) {
		t.Fatal("a retry was scheduled with no backoff at all")
	}
	if after.LastError == "" {
		t.Fatal("a failed job kept no reason")
	}

	// The backoff means it is not claimable yet, which is the point.
	if _, err := q.Claim(ctx, worker, time.Minute); !errors.Is(err, queue.ErrNoJob) {
		t.Fatalf("a job inside its backoff was claimable: %v", err)
	}

	// Second and last attempt.
	if _, err := pool.Exec(ctx, `UPDATE media_job SET run_after = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	job, err = q.Claim(ctx, worker, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	retrying, err = q.Fail(ctx, job.ID, worker, errors.New("ffmpeg exited 1"))
	if err != nil {
		t.Fatalf("second fail: %v", err)
	}
	if retrying {
		t.Fatal("a job with no attempts left was scheduled for another try")
	}
	final, err := q.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.State != queue.StateFailed {
		t.Fatalf("state is %q after the last attempt, want failed", final.State)
	}
}

func TestCompleteEndsTheJob(t *testing.T) {
	pool := testPool(t)
	q := queue.Open(pool)
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, queue.New{AssetID: newAsset(t, pool), Kind: queue.KindTranscode}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := q.Claim(ctx, "w1", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := q.Heartbeat(ctx, job.ID, "w1", time.Minute); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := q.Complete(ctx, job.ID, "w1"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	done, err := q.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if done.State != queue.StateDone {
		t.Fatalf("state is %q, want done", done.State)
	}
	if _, err := q.Claim(ctx, "w2", time.Minute); !errors.Is(err, queue.ErrNoJob) {
		t.Fatalf("a finished job was claimable again: %v", err)
	}
}
