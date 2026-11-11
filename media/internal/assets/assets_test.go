package assets_test

// Against a real Postgres, like the queue's: the transitions are single SQL
// statements with their guards in the WHERE clause, and the guards are the
// thing under test. Set AURA_MEDIA_TEST_DSN to run them.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"aura/media/internal/assets"
	"aura/media/internal/id"
	"aura/media/internal/testdb"
)

func received(t *testing.T, repo *assets.Repo) assets.Asset {
	t.Helper()
	assetID := id.New()
	a, err := repo.Create(context.Background(), assets.Asset{ID: assetID, SourceKey: "src/" + assetID, SourceBytes: 42, SourceSHA256: "sha-" + assetID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return a
}

func get(t *testing.T, repo *assets.Repo, assetID string) assets.Asset {
	t.Helper()
	a, err := repo.Get(context.Background(), assetID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return a
}

var ladder = assets.Outputs{
	DurationMs: 60_000,
	Renditions: []assets.Rendition{{Name: "720p", Width: 1280, Height: 720, BitrateKbps: 2800, Playlist: "out/x/720p/index.m3u8"}},
}

func TestAnAssetIsPlayableBeforeItIsReadyAndReadyStillMeansComplete(t *testing.T) {
	repo := assets.Open(testdb.Pool(t))
	ctx := context.Background()
	a := received(t, repo)
	if a.State != assets.StateReceived || a.Kind != assets.KindVOD || a.Progress != 0 || a.Growing || a.Address != "" {
		t.Fatalf("a new asset is %+v", a)
	}

	if err := repo.MarkProcessing(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetProgress(ctx, a.ID, 0.25); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkPublished(ctx, a.ID, "/media/out/x/master.m3u8", ladder); err != nil {
		t.Fatal(err)
	}
	early := get(t, repo, a.ID)
	// The state a 0.1.0 client polls has not moved: it is still waiting, and
	// it is right to be.
	if early.State != assets.StateProcessing {
		t.Errorf("publishing early moved the state to %q", early.State)
	}
	if early.Address != "/media/out/x/master.m3u8" || !early.Growing || early.Progress != 0.25 {
		t.Errorf("published early: %+v", early)
	}
	if len(early.Outputs.Renditions) != 1 || early.DurationMs != 60_000 {
		t.Errorf("the ladder is not readable while it grows: %+v", early.Outputs)
	}

	final := ladder
	final.Frames, final.Poster = []string{"out/x/frames/frame_00001.jpg"}, "out/x/poster.jpg"
	if err := repo.MarkReady(ctx, a.ID, "/media/out/x/master.m3u8", final); err != nil {
		t.Fatal(err)
	}
	ready := get(t, repo, a.ID)
	if ready.State != assets.StateReady || ready.Growing || ready.Progress != 1 || ready.Address != early.Address {
		t.Errorf("ready: %+v", ready)
	}
	if ready.Outputs.Poster == "" || len(ready.Outputs.Frames) != 1 {
		t.Errorf("ready outputs: %+v", ready.Outputs)
	}
}

func TestTheNewFieldsAreAdditionsToTheSameJSON(t *testing.T) {
	// What a 0.1.0 client decodes must still be there under the same names,
	// and a finished asset must not grow a field that says it is unfinished.
	repo := assets.Open(testdb.Pool(t))
	ctx := context.Background()
	a := received(t, repo)
	_ = repo.MarkProcessing(ctx, a.ID)
	_ = repo.MarkPublished(ctx, a.ID, "/media/out/x/master.m3u8", ladder)

	decode := func() map[string]any {
		raw, err := json.Marshal(get(t, repo, a.ID))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	growing := decode()
	for _, field := range []string{"id", "state", "source_bytes", "source_sha256", "address", "outputs", "created_at", "updated_at", "duration_ms"} {
		if _, ok := growing[field]; !ok {
			t.Errorf("the 0.1.0 field %q is gone from the JSON", field)
		}
	}
	if growing["growing"] != true || growing["kind"] != "vod" || growing["progress"] != 0.0 {
		t.Errorf("the additions while growing: growing=%v kind=%v progress=%v", growing["growing"], growing["kind"], growing["progress"])
	}
	if _, leaked := growing["source_key"]; leaked {
		t.Error("the source key is in the JSON")
	}

	_ = repo.MarkReady(ctx, a.ID, "/media/out/x/master.m3u8", ladder)
	ready := decode()
	if _, present := ready["growing"]; present {
		t.Errorf("a ready asset carries growing=%v", ready["growing"])
	}
	if ready["progress"] != 1.0 {
		t.Errorf("a ready asset has progress %v", ready["progress"])
	}
}

func TestProgressOnlyGoesUpAndOnlyWhileProcessing(t *testing.T) {
	repo := assets.Open(testdb.Pool(t))
	ctx := context.Background()
	a := received(t, repo)

	// Not processing yet: a late report from some earlier attempt must not
	// write onto an asset that is waiting in the queue.
	if err := repo.SetProgress(ctx, a.ID, 0.9); err != nil {
		t.Fatal(err)
	}
	if got := get(t, repo, a.ID).Progress; got != 0 {
		t.Errorf("progress was written to an asset nobody is processing: %v", got)
	}

	_ = repo.MarkProcessing(ctx, a.ID)
	for _, step := range []struct{ write, want float64 }{
		{0.10, 0.10}, {0.40, 0.40}, {0.30, 0.40}, {0.41, 0.41}, {-3, 0.41}, {7, 1},
	} {
		if err := repo.SetProgress(ctx, a.ID, step.write); err != nil {
			t.Fatal(err)
		}
		if got := get(t, repo, a.ID).Progress; got != step.want {
			t.Errorf("after writing %v progress is %v, want %v", step.write, got, step.want)
		}
	}
	// An asset that does not exist is not an error worth failing a job over.
	if err := repo.SetProgress(ctx, "no-such-asset", 0.5); err != nil {
		t.Errorf("progress for a missing asset: %v", err)
	}

	// A new attempt starts from zero.
	_ = repo.Requeued(ctx, a.ID, "ffmpeg failed")
	_ = repo.MarkProcessing(ctx, a.ID)
	if got := get(t, repo, a.ID).Progress; got != 0 {
		t.Errorf("a second attempt starts at %v", got)
	}
}

func TestAnEarlyAddressDoesNotOutliveTheAttemptThatPublishedIt(t *testing.T) {
	ctx := context.Background()
	const address = "/media/out/x/master.m3u8"

	cases := map[string]struct {
		end  func(repo *assets.Repo, assetID string) error
		want assets.State
	}{
		"the job will be retried":                      {func(r *assets.Repo, i string) error { return r.Requeued(ctx, i, "ffmpeg failed") }, assets.StateReceived},
		"the job failed for good":                      {func(r *assets.Repo, i string) error { return r.MarkFailed(ctx, i, "ffmpeg failed") }, assets.StateFailed},
		"another way of encoding is about to be tried": {func(r *assets.Repo, i string) error { return r.Unpublish(ctx, i) }, assets.StateProcessing},
		"a new attempt begins":                         {func(r *assets.Repo, i string) error { return r.MarkProcessing(ctx, i) }, assets.StateProcessing},
	}
	repo := assets.Open(testdb.Pool(t))
	for name, c := range cases {
		a := received(t, repo)
		_ = repo.MarkProcessing(ctx, a.ID)
		_ = repo.SetProgress(ctx, a.ID, 0.5)
		if err := repo.MarkPublished(ctx, a.ID, address, ladder); err != nil {
			t.Fatal(err)
		}
		if err := c.end(repo, a.ID); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := get(t, repo, a.ID)
		// A player left holding the address of a package nobody is writing
		// any more would wait on it for ever.
		if got.State != c.want || got.Address != "" || got.Growing {
			t.Errorf("%s: state=%q address=%q growing=%v", name, got.State, got.Address, got.Growing)
		}
	}
}

func TestOnlyAnAssetBeingProcessedCanBePublishedEarly(t *testing.T) {
	// A worker that lost its lease and has not noticed must not put an
	// address back on an asset another worker has since finished or failed.
	repo := assets.Open(testdb.Pool(t))
	ctx := context.Background()

	waiting := received(t, repo)
	if err := repo.MarkPublished(ctx, waiting.ID, "/x", ladder); !errors.Is(err, assets.ErrNotFound) {
		t.Errorf("published an asset nobody is processing: %v", err)
	}

	failed := received(t, repo)
	_ = repo.MarkProcessing(ctx, failed.ID)
	_ = repo.MarkFailed(ctx, failed.ID, "gave up")
	if err := repo.MarkPublished(ctx, failed.ID, "/x", ladder); !errors.Is(err, assets.ErrNotFound) {
		t.Errorf("published a failed asset: %v", err)
	}
	if got := get(t, repo, failed.ID); got.Address != "" || got.State != assets.StateFailed {
		t.Errorf("the failed asset is now %+v", got)
	}

	done := received(t, repo)
	_ = repo.MarkProcessing(ctx, done.ID)
	_ = repo.MarkReady(ctx, done.ID, "/media/out/final/master.m3u8", ladder)
	if err := repo.MarkPublished(ctx, done.ID, "/stale", ladder); !errors.Is(err, assets.ErrNotFound) {
		t.Errorf("a late worker re-published a ready asset: %v", err)
	}
	if err := repo.Unpublish(ctx, done.ID); !errors.Is(err, assets.ErrNotFound) {
		t.Errorf("a late worker unpublished a ready asset: %v", err)
	}
	if got := get(t, repo, done.ID); got.Address != "/media/out/final/master.m3u8" || got.State != assets.StateReady {
		t.Errorf("the ready asset is now %+v", got)
	}
}

func TestALiveAssetIsBornProcessingWithNoSource(t *testing.T) {
	repo := assets.Open(testdb.Pool(t))
	ctx := context.Background()
	assetID := id.New()
	a, err := repo.CreateLive(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != assets.StateProcessing || a.Kind != assets.KindLive || a.SourceKey != "" || a.Growing || a.Address != "" {
		t.Errorf("a new live asset is %+v", a)
	}
	// The same id twice is a bug somewhere, and must not quietly reset a
	// stream that is already being watched.
	if _, err := repo.CreateLive(ctx, assetID); err == nil {
		t.Error("a second live asset with the same id was created")
	}
	if _, err := repo.CreateLive(ctx, ""); err == nil {
		t.Error("a live asset with no id was created")
	}
	// It has no digest, so it can never be what an upload deduplicates to.
	if _, err := repo.BySourceDigest(ctx, ""); !errors.Is(err, assets.ErrNotFound) {
		t.Errorf("an empty digest matched something: %v", err)
	}
	if err := repo.MarkPublished(ctx, assetID, "/media/out/l/master.m3u8", ladder); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkReady(ctx, assetID, "/media/out/l/master.m3u8", ladder); err != nil {
		t.Fatal(err)
	}
	if got := get(t, repo, assetID); got.Kind != assets.KindLive || got.State != assets.StateReady || got.Growing {
		t.Errorf("a finished stream is %+v", got)
	}
}
