package db_test

// The upgrade is tested against the thing being upgraded. testdata holds the
// schema exactly as 0.1.0 shipped it; the test builds a database with that,
// fills it the way a 0.1.0 node would have, and then starts today's code
// against it. Production databases are that database, not an empty one.

import (
	"context"
	_ "embed"
	"strings"
	"sync"
	"testing"
	"time"

	"aura/media/internal/assets"
	"aura/media/internal/db"
	"aura/media/internal/live"
	"aura/media/internal/queue"
	"aura/media/internal/testdb"
)

//go:embed testdata/schema_0.1.0.sql
var schema010 string

func TestADatabaseFrom010IsBroughtForwardWithItsRowsIntact(t *testing.T) {
	pool := testdb.Empty(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, schema010); err != nil {
		t.Fatalf("applying the 0.1.0 schema: %v", err)
	}
	// Rows as a 0.1.0 node wrote them: only the columns it knew, in every
	// state an asset can be left in.
	const outputs = `{"renditions":[{"name":"720p","width":1280,"height":720,"bitrate_kbps":2800,"playlist":"out/OLDREADY/720p/index.m3u8"}],"frames":["out/OLDREADY/frames/frame_00001.jpg"],"poster":"out/OLDREADY/poster.jpg","duration_ms":61000}`
	for _, q := range []string{
		`INSERT INTO media_asset (id, state, source_key, source_bytes, source_sha256, mime, duration_ms, address, outputs)
		 VALUES ('OLDREADY', 'ready', 'src/OLDREADY', 1234, 'abc', 'video/mp4', 61000, '/media/out/OLDREADY/master.m3u8', '` + outputs + `')`,
		`INSERT INTO media_asset (id, state, source_key) VALUES ('OLDRECEIVED', 'received', 'src/OLDRECEIVED')`,
		`INSERT INTO media_asset (id, state, source_key) VALUES ('OLDPROCESSING', 'processing', 'src/OLDPROCESSING')`,
		`INSERT INTO media_asset (id, state, source_key, error) VALUES ('OLDFAILED', 'failed', 'src/OLDFAILED', 'ffmpeg failed')`,
		`INSERT INTO media_job (asset_id, kind, state, attempts, idem) VALUES ('OLDREADY', 'transcode', 'done', 1, 'key-1')`,
		`INSERT INTO media_job (asset_id, kind, idem, args) VALUES ('OLDRECEIVED', 'transcode', 'key-2', '{"priority":"5"}')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("writing a 0.1.0 row: %v", err)
		}
	}

	// The new binary starts. Twice, because it runs on every start.
	for i := 0; i < 2; i++ {
		if err := db.Migrate(ctx, pool); err != nil {
			t.Fatalf("migration %d against a 0.1.0 database: %v", i+1, err)
		}
	}

	repo := assets.Open(pool)
	ready, err := repo.Get(ctx, "OLDREADY")
	if err != nil {
		t.Fatalf("reading a 0.1.0 asset: %v", err)
	}
	if ready.State != assets.StateReady || ready.Address != "/media/out/OLDREADY/master.m3u8" || ready.DurationMs != 61000 ||
		ready.SourceBytes != 1234 || ready.SourceSHA256 != "abc" || ready.MIME != "video/mp4" {
		t.Errorf("a 0.1.0 ready asset reads back changed: %+v", ready)
	}
	if len(ready.Outputs.Renditions) != 1 || ready.Outputs.Renditions[0].Width != 1280 || ready.Outputs.Poster == "" || len(ready.Outputs.Frames) != 1 {
		t.Errorf("its outputs read back changed: %+v", ready.Outputs)
	}
	// It was finished before anyone counted progress. It reads as finished.
	if ready.Progress != 1 || ready.Growing {
		t.Errorf("an asset 0.1.0 finished reads as progress=%v growing=%v", ready.Progress, ready.Growing)
	}
	if ready.Kind != assets.KindVOD {
		t.Errorf("an uploaded asset reads as kind %q", ready.Kind)
	}
	for id, want := range map[string]assets.State{"OLDRECEIVED": assets.StateReceived, "OLDPROCESSING": assets.StateProcessing, "OLDFAILED": assets.StateFailed} {
		a, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if a.State != want || a.Progress != 0 || a.Growing || a.Address != "" || a.Kind != assets.KindVOD {
			t.Errorf("%s reads back as %+v", id, a)
		}
	}
	list, err := repo.List(ctx, 0)
	if err != nil || len(list) != 4 {
		t.Errorf("listing the old assets: %d, %v", len(list), err)
	}
	// Deduplication still finds an asset uploaded under 0.1.0.
	if found, err := repo.BySourceDigest(ctx, "abc"); err != nil || found.ID != "OLDREADY" {
		t.Errorf("the old asset is not found by its digest: %+v, %v", found, err)
	}

	// The old job is claimable by a new worker, with its idempotency key and
	// its args.
	jobs := queue.Open(pool)
	job, err := jobs.Claim(ctx, "w-new", time.Minute)
	if err != nil {
		t.Fatalf("claiming a job queued by 0.1.0: %v", err)
	}
	if job.AssetID != "OLDRECEIVED" || job.Kind != queue.KindTranscode || job.Idem != "key-2" || job.Args["priority"] != "5" {
		t.Errorf("the 0.1.0 job reads back as %+v", job)
	}
	if existing, err := jobs.ByIdem(ctx, "key-1"); err != nil || existing.State != queue.StateDone {
		t.Errorf("the finished job is not found by its key: %+v, %v", existing, err)
	}

	// And what 0.2.0 does to an old row works: the worker that claimed it can
	// take it through the new transitions.
	if err := repo.MarkProcessing(ctx, "OLDRECEIVED"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetProgress(ctx, "OLDRECEIVED", 0.4); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkPublished(ctx, "OLDRECEIVED", "/media/out/OLDRECEIVED/master.m3u8", assets.Outputs{}); err != nil {
		t.Fatal(err)
	}
	a, _ := repo.Get(ctx, "OLDRECEIVED")
	if a.Progress != 0.4 || !a.Growing || a.Address == "" {
		t.Errorf("an old row taken through the new transitions: %+v", a)
	}

	// The table that did not exist is there and usable.
	if _, err := live.OpenRepo(pool).Create(ctx, "S1", live.HashKey("S1_secret"), "A1"); err != nil {
		t.Errorf("media_live after the upgrade: %v", err)
	}
}

func TestA010BinaryStillWorksAgainstTheUpgradedDatabase(t *testing.T) {
	// A rolling deploy has both versions pointed at one database for a while.
	// These are 0.1.0's statements, verbatim: they name their columns, so the
	// new ones must all have defaults and none of the old ones may have moved.
	pool := testdb.Pool(t)
	ctx := context.Background()
	const oldColumns = `id, state, source_key, source_bytes, source_sha256, mime,
		duration_ms, address, outputs, error, created_at, updated_at`

	if _, err := pool.Exec(ctx, `INSERT INTO media_asset (id, state, source_key, source_bytes, source_sha256, mime)
		VALUES ($1, $2, $3, $4, $5, $6)`, "A", "received", "src/A", int64(10), "sha", "video/mp4"); err != nil {
		t.Fatalf("0.1.0's INSERT is refused by the new schema: %v", err)
	}
	for _, q := range []string{
		`UPDATE media_asset SET state = 'processing', updated_at = now() WHERE id = 'A'`,
		`UPDATE media_asset SET state = 'ready', address = '/media/out/A/master.m3u8', outputs = '{}', duration_ms = 5, error = '', updated_at = now() WHERE id = 'A'`,
		`INSERT INTO media_job (asset_id, kind, priority, max_attempts, idem, args) VALUES ('A', 'transcode', 100, 3, 'k', '{}')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("a 0.1.0 statement is refused by the new schema: %v\n%s", err, q)
		}
	}
	var state, address string
	if err := pool.QueryRow(ctx, `SELECT state, address FROM (SELECT `+oldColumns+` FROM media_asset WHERE id = 'A') old`).Scan(&state, &address); err != nil {
		t.Fatalf("0.1.0's SELECT fails against the new schema: %v", err)
	}
	// And the new reader agrees with what the old writer left.
	a, err := assets.Open(pool).Get(ctx, "A")
	if err != nil || a.State != assets.StateReady || a.Progress != 1 || a.Growing {
		t.Errorf("an asset a 0.1.0 worker finished reads as %+v, %v", a, err)
	}
}

func TestNodesStartingTogetherDoNotRaceTheSchema(t *testing.T) {
	// Every node applies the schema on start, and a deploy starts several at
	// once. IF NOT EXISTS does not make that safe on its own: two sessions can
	// both find a table missing and both create it, and the loser fails.
	pool := testdb.Empty(t)
	ctx := context.Background()
	const nodes = 8
	var wg sync.WaitGroup
	errs := make(chan error, nodes)
	for i := 0; i < nodes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- db.Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a node failed to start because another was starting: %v", err)
		}
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('media_asset', 'media_job', 'media_live')`).Scan(&tables); err != nil || tables != 3 {
		t.Errorf("%d of 3 tables after concurrent migration (%v)", tables, err)
	}
}

func TestTheSchemaOnlyAdds(t *testing.T) {
	// Not a parse of SQL, a tripwire: the statements that would make an
	// upgrade destructive or a rolling deploy impossible do not appear at all.
	schema := db.Schema()
	for _, banned := range []string{"DROP ", "RENAME ", "ALTER COLUMN", "TRUNCATE", "DELETE FROM"} {
		if containsFold(schema, banned) {
			t.Errorf("the schema contains %q; every change must be an addition", banned)
		}
	}
	if !containsFold(schema, "ADD COLUMN IF NOT EXISTS") {
		t.Error("the new columns are not added idempotently")
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToUpper(s), strings.ToUpper(sub))
}

func TestARestartDoesNotNeedTheTableToItself(t *testing.T) {
	// The schema is applied on every start, and something is often holding
	// media_asset open: a backup reads it for minutes. A start that asked for
	// an exclusive lock each time would queue behind the backup, and every
	// query the running nodes make would queue behind the start.
	pool := testdb.Pool(t) // already at the current schema
	ctx := context.Background()

	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	// What pg_dump holds for the length of a backup.
	if _, err := reader.Exec(ctx, `LOCK TABLE media_asset IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- db.Migrate(ctx, pool) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a restart during a backup failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a restart is waiting on a table lock while a reader holds the table; it should not need one")
	}
}

func TestTheOneUpgradeThatNeedsTheTableGivesUpRatherThanQueueing(t *testing.T) {
	// The first start against a 0.1.0 database does have to alter the table.
	// If it cannot get it, it must fail and say why, not wait for ever with
	// everyone else's queries stacked up behind it.
	pool := testdb.Empty(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, schema010); err != nil {
		t.Fatal(err)
	}
	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Exec(ctx, `LOCK TABLE media_asset IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = db.Migrate(ctx, pool)
	if err == nil {
		t.Fatal("the upgrade altered a table another transaction was holding")
	}
	if waited := time.Since(start); waited < 8*time.Second || waited > 20*time.Second {
		t.Errorf("gave up after %s, want about ten seconds", waited.Round(time.Second))
	}
	if !strings.Contains(err.Error(), "Nothing was changed") || !strings.Contains(err.Error(), "start again") {
		t.Errorf("the failure does not say what to do: %v", err)
	}
	// And nothing was changed: the old schema is intact, and 0.1.0's
	// statements still run against it.
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'media_asset'`).Scan(&columns); err != nil || columns != 12 {
		t.Errorf("media_asset has %d columns after a failed upgrade, want 0.1.0's 12 (%v)", columns, err)
	}
	var live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = 'media_live'`).Scan(&live); err != nil || live != 0 {
		t.Errorf("half an upgrade was left behind: media_live exists (%v)", err)
	}

	// The backup finishes. The same start now succeeds.
	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("the upgrade after the table was released: %v", err)
	}
	if _, err := assets.Open(pool).Create(ctx, assets.Asset{ID: "A", SourceKey: "src/A"}); err != nil {
		t.Errorf("the upgraded database: %v", err)
	}
}
