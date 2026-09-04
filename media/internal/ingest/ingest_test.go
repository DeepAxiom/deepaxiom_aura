package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aura/media/internal/assets"
	"aura/media/internal/queue"
	"aura/media/internal/store"
)

// fakeCatalogue and fakeJobs stand in for Postgres. What is being tested here
// is this package's rules, not the database's.
type fakeCatalogue struct {
	byID      map[string]assets.Asset
	byDigest  map[string]assets.Asset
	createErr error
}

func newCatalogue() *fakeCatalogue {
	return &fakeCatalogue{byID: map[string]assets.Asset{}, byDigest: map[string]assets.Asset{}}
}

func (f *fakeCatalogue) Create(_ context.Context, a assets.Asset) (assets.Asset, error) {
	if f.createErr != nil {
		return assets.Asset{}, f.createErr
	}
	a.State = assets.StateReceived
	f.byID[a.ID] = a
	f.byDigest[a.SourceSHA256] = a
	return a, nil
}

func (f *fakeCatalogue) Get(_ context.Context, assetID string) (assets.Asset, error) {
	if a, ok := f.byID[assetID]; ok {
		return a, nil
	}
	return assets.Asset{}, assets.ErrNotFound
}

func (f *fakeCatalogue) Delete(_ context.Context, assetID string) error {
	a, ok := f.byID[assetID]
	if !ok {
		return assets.ErrNotFound
	}
	delete(f.byID, assetID)
	delete(f.byDigest, a.SourceSHA256)
	return nil
}

func (f *fakeCatalogue) BySourceDigest(_ context.Context, sha string) (assets.Asset, error) {
	if a, ok := f.byDigest[sha]; ok {
		return a, nil
	}
	return assets.Asset{}, assets.ErrNotFound
}

type fakeJobs struct {
	jobs     []queue.Job
	next     int64
	enqueErr error
}

func (f *fakeJobs) Enqueue(_ context.Context, n queue.New) (queue.Job, error) {
	if f.enqueErr != nil {
		return queue.Job{}, f.enqueErr
	}
	f.next++
	job := queue.Job{ID: f.next, AssetID: n.AssetID, Kind: n.Kind, State: queue.StateQueued, Idem: n.Idem}
	f.jobs = append(f.jobs, job)
	return job, nil
}

func (f *fakeJobs) ByIdem(_ context.Context, idem string) (queue.Job, error) {
	if idem == "" {
		return queue.Job{}, queue.ErrNoJob
	}
	for _, j := range f.jobs {
		if j.Idem == idem {
			return j, nil
		}
	}
	return queue.Job{}, queue.ErrNoJob
}

func (f *fakeJobs) ForAsset(_ context.Context, assetID string) ([]queue.Job, error) {
	var out []queue.Job
	for _, j := range f.jobs {
		if j.AssetID == assetID {
			out = append(out, j)
		}
	}
	return out, nil
}

func newIngestor(t *testing.T) (*Ingestor, *fakeCatalogue, *fakeJobs) {
	t.Helper()
	objects, err := store.NewFS(t.TempDir(), "", "/media")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cat, jobs := newCatalogue(), &fakeJobs{}
	return &Ingestor{Assets: cat, Queue: jobs, Store: objects}, cat, jobs
}

func TestFromReaderStoresQueuesAndHashes(t *testing.T) {
	in, cat, jobs := newIngestor(t)
	res, err := in.FromReader(context.Background(), strings.NewReader("pretend this is an mp4"), Request{MIME: "video/mp4"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Asset.ID == "" || res.Job.ID == 0 {
		t.Fatalf("ingest returned no asset or no job: %+v", res)
	}
	if res.Asset.SourceBytes != 22 {
		t.Errorf("stored %d bytes, want 22", res.Asset.SourceBytes)
	}
	if res.Asset.SourceSHA256 == "" {
		t.Error("no digest recorded, so nothing can be deduplicated later")
	}
	if len(cat.byID) != 1 || len(jobs.jobs) != 1 {
		t.Errorf("catalogue has %d assets and %d jobs, want 1 and 1", len(cat.byID), len(jobs.jobs))
	}
	stored, _, err := in.Store.Open(context.Background(), res.Asset.SourceKey)
	if err != nil {
		t.Errorf("the source is not in the store: %v", err)
		return
	}
	stored.Close()
}

func TestFromReaderRefusesAnEmptySource(t *testing.T) {
	in, _, jobs := newIngestor(t)
	if _, err := in.FromReader(context.Background(), strings.NewReader(""), Request{}); err == nil {
		t.Fatal("an empty upload was accepted")
	}
	if len(jobs.jobs) != 0 {
		t.Error("an empty upload queued work")
	}
}

// A source over the limit must fail, not arrive truncated: half a video stored
// as if it were whole is worse than a refusal, because nothing downstream can
// tell the difference.
func TestFromReaderRefusesRatherThanTruncates(t *testing.T) {
	in, cat, jobs := newIngestor(t)
	in.MaxBytes = 64
	_, err := in.FromReader(context.Background(), strings.NewReader(strings.Repeat("x", 5000)), Request{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error is %v, want ErrTooLarge", err)
	}
	if len(cat.byID) != 0 || len(jobs.jobs) != 0 {
		t.Error("an oversized upload left an asset or a job behind")
	}
}

func TestFromReaderDeduplicatesIdenticalBytes(t *testing.T) {
	in, cat, jobs := newIngestor(t)
	ctx := context.Background()
	first, err := in.FromReader(ctx, strings.NewReader("same bytes"), Request{})
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := in.FromReader(ctx, strings.NewReader("same bytes"), Request{})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if !second.Deduplicated {
		t.Error("the same file was ingested twice without being recognised")
	}
	if second.Asset.ID != first.Asset.ID {
		t.Errorf("second ingest made asset %s, want the existing %s", second.Asset.ID, first.Asset.ID)
	}
	// An encode is minutes of a pinned core. Doing it twice for one file is the
	// most expensive possible way to arrive at the same address.
	if len(jobs.jobs) != 1 {
		t.Errorf("%d jobs queued for one distinct file, want 1", len(jobs.jobs))
	}
	if len(cat.byID) != 1 {
		t.Errorf("%d assets for one distinct file, want 1", len(cat.byID))
	}
}

// The bytes are stored and the row exists, but nothing will ever run. A caller
// told "accepted" here would wait forever for an address.
func TestFromReaderSaysSoWhenNothingWillRun(t *testing.T) {
	in, _, jobs := newIngestor(t)
	jobs.enqueErr = errors.New("the queue is unreachable")
	_, err := in.FromReader(context.Background(), strings.NewReader("bytes"), Request{})
	if err == nil {
		t.Fatal("an asset that was never queued was reported as accepted")
	}
	if !strings.Contains(err.Error(), "not queued") {
		t.Errorf("error %q does not say the work was not queued", err)
	}
}

func TestFromURIReadsAFileAndRefusesTheRest(t *testing.T) {
	in, _, _ := newIngestor(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(path, []byte("file bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := "file:///" + filepath.ToSlash(path)
	res, err := in.FromURI(context.Background(), uri, Request{})
	if err != nil {
		t.Fatalf("file URI: %v", err)
	}
	if res.Asset.SourceBytes != 10 {
		t.Errorf("stored %d bytes, want 10", res.Asset.SourceBytes)
	}

	// HTTP is off unless the operator turned it on: a node that fetches any URI
	// it is handed is a node anyone can point at anything.
	if _, err := in.FromURI(context.Background(), "https://example.invalid/x.mp4", Request{}); err == nil {
		t.Error("fetched over HTTP with no client configured")
	}
	// And a scheme nobody thought about is refused by name, not by surprise.
	if _, err := in.FromURI(context.Background(), "s3://bucket/key", Request{}); err == nil {
		t.Error("accepted a scheme this node cannot fetch")
	}
}

// The failure this was written for: the same key with different bytes used to
// store a new asset and hand back the FIRST asset's job. Nothing would ever
// encode the new one, and the caller polled a row that could not move.
func TestSameIdempotencyKeyNeverAnswersWithAnotherAssetsWork(t *testing.T) {
	in, cat, jobs := newIngestor(t)
	ctx := context.Background()

	first, err := in.FromReader(ctx, strings.NewReader("recording one"), Request{Idem: "e2e-1"})
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := in.FromReader(ctx, strings.NewReader("a completely different recording"), Request{Idem: "e2e-1"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	if !second.Deduplicated {
		t.Error("the reused key was not recognised as already answered")
	}
	if second.Asset.ID != first.Asset.ID {
		t.Errorf("the reused key produced asset %s beside %s", second.Asset.ID, first.Asset.ID)
	}
	if second.Job.AssetID != second.Asset.ID {
		t.Errorf("job %d belongs to asset %s but was returned for %s",
			second.Job.ID, second.Job.AssetID, second.Asset.ID)
	}
	if len(jobs.jobs) != 1 || len(cat.byID) != 1 {
		t.Errorf("%d jobs and %d assets after a reused key, want 1 and 1", len(jobs.jobs), len(cat.byID))
	}
	// And the second body was never stored: the key is checked before a byte is
	// read, so a redelivered upload costs nothing but the request.
	sources, err := os.ReadDir(filepath.Join(in.Store.(*store.FS).Root, "src"))
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	if len(sources) != 1 {
		t.Errorf("%d sources stored for one answered key, want 1", len(sources))
	}
}
