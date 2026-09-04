package api_test

// These run against a real Postgres, for the same reason the queue's do: what
// is being checked is a whole request path, and a fake repository would agree
// with whatever this code did. Set AURA_MEDIA_TEST_DSN to run them.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"aura/media/internal/api"
	"aura/media/internal/assets"
	"aura/media/internal/db"
	"aura/media/internal/ingest"
	"aura/media/internal/queue"
	"aura/media/internal/store"
)

const testToken = "test-token-that-is-long-enough"

func newServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, store.Store) {
	t.Helper()
	dsn := os.Getenv("AURA_MEDIA_TEST_DSN")
	if dsn == "" {
		t.Skip("AURA_MEDIA_TEST_DSN is not set; skipping the API's Postgres tests")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE media_job, media_asset RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	objects, err := store.NewFS(t.TempDir(), "", "/media")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	repo, jobs := assets.Open(pool), queue.Open(pool)
	srv := &api.Server{
		Assets:      repo,
		Queue:       jobs,
		Store:       objects,
		Ingest:      &ingest.Ingestor{Assets: repo, Queue: jobs, Store: objects},
		Token:       testToken,
		MediaPrefix: "/media",
		Version:     "test",
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		pool.Close()
	})
	return ts, pool, objects
}

func do(t *testing.T, ts *httptest.Server, method, path, token, body string, headers map[string]string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestControlSurfaceRefusesWithoutTheToken(t *testing.T) {
	ts, _, _ := newServer(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/assets"},
		{http.MethodGet, "/v1/assets"},
		{http.MethodGet, "/v1/assets/whatever"},
		{http.MethodGet, "/v1/queue"},
		{http.MethodGet, "/v1/version"},
	} {
		resp := do(t, ts, c.method, c.path, "", "x", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d with no token, want 401", c.method, c.path, resp.StatusCode)
		}
	}
	// A wrong token is refused the same way as none.
	if resp := do(t, ts, http.MethodGet, "/v1/queue", "not-the-token", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an invented token answered %d, want 401", resp.StatusCode)
	}
}

func TestLivenessAndReadinessAreOpen(t *testing.T) {
	ts, _, _ := newServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if resp := do(t, ts, http.MethodGet, path, "", "", nil); resp.StatusCode != http.StatusOK {
			t.Errorf("%s answered %d with no token, want 200", path, resp.StatusCode)
		}
	}
}

func TestUploadIsAcceptedAndReadableBack(t *testing.T) {
	ts, _, _ := newServer(t)
	resp := do(t, ts, http.MethodPost, "/v1/assets", testToken, "pretend mp4 bytes",
		map[string]string{"Content-Type": "video/mp4"})
	// 202, not 201: the asset exists, the address does not yet.
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upload answered %d, want 202", resp.StatusCode)
	}
	var created struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Address  string `json:"address"`
		JobID    int64  `json:"job_id"`
		JobState string `json:"job_state"`
	}
	decode(t, resp, &created)
	if created.ID == "" || created.JobID == 0 {
		t.Fatalf("upload returned no asset id or no job: %+v", created)
	}
	if created.Address != "" {
		t.Errorf("an asset that has not been encoded already has an address: %q", created.Address)
	}
	if got := resp.Header.Get("Location"); got != "/v1/assets/"+created.ID {
		t.Errorf("Location is %q, want /v1/assets/%s", got, created.ID)
	}

	read := do(t, ts, http.MethodGet, "/v1/assets/"+created.ID, testToken, "", nil)
	if read.StatusCode != http.StatusOK {
		t.Fatalf("reading the asset answered %d", read.StatusCode)
	}
	var fetched struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		JobState string `json:"job_state"`
	}
	decode(t, read, &fetched)
	if fetched.ID != created.ID {
		t.Errorf("read back %q, want %q", fetched.ID, created.ID)
	}
	if fetched.State != string(assets.StateReceived) {
		t.Errorf("state is %q, want received", fetched.State)
	}
	if fetched.JobState != string(queue.StateQueued) {
		t.Errorf("job state is %q, want queued", fetched.JobState)
	}

	if missing := do(t, ts, http.MethodGet, "/v1/assets/01NOPE", testToken, "", nil); missing.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown asset answered %d, want 404", missing.StatusCode)
	}
}

// An at-least-once caller redelivers. That must find the work already under way,
// not start a second encode of the same file.
func TestIdempotencyKeyDoesNotStartASecondEncode(t *testing.T) {
	ts, pool, _ := newServer(t)
	headers := map[string]string{"Content-Type": "video/mp4", "Idempotency-Key": "sess-1:asset_in:3"}

	first := do(t, ts, http.MethodPost, "/v1/assets", testToken, "some bytes", headers)
	second := do(t, ts, http.MethodPost, "/v1/assets", testToken, "some bytes", headers)
	if first.StatusCode != http.StatusAccepted {
		t.Errorf("the first upload answered %d, want 202", first.StatusCode)
	}
	// 200, not 202: nothing new was queued, and a client that reused one key for
	// two different videos is being handed the first one's work.
	if second.StatusCode != http.StatusOK {
		t.Errorf("a redelivered upload answered %d, want 200", second.StatusCode)
	}
	var a, b struct {
		ID           string `json:"id"`
		JobID        int64  `json:"job_id"`
		Deduplicated bool   `json:"deduplicated"`
	}
	decode(t, first, &a)
	decode(t, second, &b)
	if a.JobID != b.JobID {
		t.Errorf("a redelivered upload made job %d as well as %d", b.JobID, a.JobID)
	}
	if a.ID != b.ID {
		t.Errorf("a redelivered upload made asset %s as well as %s", b.ID, a.ID)
	}
	var jobs int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_job`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("%d jobs in the table, want 1", jobs)
	}
	if !b.Deduplicated {
		t.Error("the second answer does not say it queued nothing")
	}
}

// The public tree is output only. A source is the clip before anything
// de-identified it, and it must not be fetchable by guessing an id.
func TestOnlyPackagedOutputIsPublic(t *testing.T) {
	ts, _, objects := newServer(t)
	ctx := context.Background()
	if _, err := objects.Put(ctx, "src/01SOURCE", strings.NewReader("the original recording")); err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Put(ctx, "out/01ASSET/master.m3u8", strings.NewReader("#EXTM3U")); err != nil {
		t.Fatal(err)
	}

	if resp := do(t, ts, http.MethodGet, "/media/src/01SOURCE", "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a source was served over the public tree with status %d", resp.StatusCode)
	}
	// Even with the operator's own token: this route serves the public zone,
	// and a source is not in it.
	if resp := do(t, ts, http.MethodGet, "/media/src/01SOURCE", testToken, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a source was served to a token holder over the public tree: %d", resp.StatusCode)
	}

	resp := do(t, ts, http.MethodGet, "/media/out/01ASSET/master.m3u8", "", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the packaged output answered %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Errorf("playlist content type is %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "#EXTM3U" {
		t.Errorf("served %q", body)
	}

	if resp := do(t, ts, http.MethodGet, "/media/out/01ASSET/missing.m4s", "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a missing segment answered %d, want 404", resp.StatusCode)
	}
}

func TestJSONBodyMustNameASource(t *testing.T) {
	ts, _, _ := newServer(t)
	resp := do(t, ts, http.MethodPost, "/v1/assets", testToken, `{"mime":"video/mp4"}`,
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a JSON body with no uri answered %d, want 400", resp.StatusCode)
	}
	var problem struct {
		Error string `json:"error"`
	}
	decode(t, resp, &problem)
	if !strings.Contains(problem.Error, "uri") {
		t.Errorf("the refusal does not say what is missing: %q", problem.Error)
	}
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
}
