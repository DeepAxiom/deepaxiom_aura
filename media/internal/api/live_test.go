package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aura/media/internal/api"
	"aura/media/internal/assets"
	"aura/media/internal/ingest"
	"aura/media/internal/live"
	"aura/media/internal/queue"
	"aura/media/internal/store"
	"aura/media/internal/testdb"
)

func TestPlaylistsStillBeingWrittenAreNotCachedLikeFinishedOnes(t *testing.T) {
	// No database: this is the rule itself.
	const header = "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:41\n#EXT-X-MAP:URI=\"init_0.mp4\"\n#EXTINF:2.000000,\nseg_00041.m4s\n"
	cases := []struct {
		name string
		text string
		want string
	}{
		// A second: a player polls once a segment, and a CDN in front turns
		// every viewer's poll into one request a second to this node.
		{"a live window", header, "public, max-age=1"},
		{"an encode publishing as it goes", strings.Replace(header, "#EXT-X-MEDIA-SEQUENCE:41\n", "#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n", 1), "public, max-age=1"},
		// What every playlist was before 0.2.0, and what it still gets.
		{"a finished VOD", header + "#EXT-X-ENDLIST\n", "public, max-age=60"},
		{"a master playlist", "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"avc1\"\n720p/index.m3u8\n", "public, max-age=60"},
		{"the bare playlist the older tests store", "#EXTM3U", "public, max-age=60"},
		{"nothing", "", "public, max-age=60"},
	}
	for _, c := range cases {
		if got := api.PlaylistCacheControl([]byte(c.text)); got != c.want {
			t.Errorf("%s: Cache-Control %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCacheHeadersAsServed(t *testing.T) {
	ts, _, objects := newServer(t)
	ctx := context.Background()
	const growing = "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.000000,\nseg_00000.m4s\n"
	put := func(key, body string) {
		t.Helper()
		if _, err := objects.Put(ctx, key, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	put("out/A/master.m3u8", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n720p/index.m3u8\n")
	put("out/A/720p/index.m3u8", growing)
	put("out/A/720p/seg_00000.m4s", "segment bytes")
	put("out/B/720p/index.m3u8", growing+"#EXT-X-ENDLIST\n")

	cases := map[string]string{
		"/media/out/A/720p/index.m3u8":    "public, max-age=1",
		"/media/out/B/720p/index.m3u8":    "public, max-age=60",
		"/media/out/A/master.m3u8":        "public, max-age=60",
		"/media/out/A/720p/seg_00000.m4s": "public, max-age=31536000, immutable",
	}
	for path, want := range cases {
		resp := do(t, ts, http.MethodGet, path, "", "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control %q, want %q", path, got, want)
		}
	}
	// The body is what was stored, byte for byte, and the length is its own.
	resp := do(t, ts, http.MethodGet, "/media/out/A/720p/index.m3u8", "", "", nil)
	body, _ := io.ReadAll(resp.Body)
	if string(body) != growing || resp.ContentLength != int64(len(growing)) {
		t.Errorf("served %q with Content-Length %d", body, resp.ContentLength)
	}
	// The moment the playlist is closed, the next fetch says so.
	put("out/A/720p/index.m3u8", growing+"#EXT-X-ENDLIST\n")
	if got := do(t, ts, http.MethodGet, "/media/out/A/720p/index.m3u8", "", "", nil).Header.Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("after ENDLIST: Cache-Control %q", got)
	}
}

func TestAnAssetStillEncodingShowsItsAddressAndProgress(t *testing.T) {
	ts, pool, _ := newServer(t)
	ctx := context.Background()
	repo := assets.Open(pool)

	resp := do(t, ts, http.MethodPost, "/v1/assets", testToken, "some bytes", map[string]string{"Content-Type": "video/mp4"})
	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	assetID, _ := created["id"].(string)
	// The fields are there from the first answer, saying "nothing yet".
	if created["progress"] != 0.0 || created["kind"] != "vod" {
		t.Errorf("a just-uploaded asset: progress=%v kind=%v", created["progress"], created["kind"])
	}
	if _, present := created["growing"]; present {
		t.Errorf("a just-uploaded asset says growing=%v", created["growing"])
	}

	// What the worker does a few seconds into the encode.
	if err := repo.MarkProcessing(ctx, assetID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetProgress(ctx, assetID, 0.3); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkPublished(ctx, assetID, "/media/out/"+assetID+"/master.m3u8", assets.Outputs{DurationMs: 9000}); err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.NewDecoder(do(t, ts, http.MethodGet, "/v1/assets/"+assetID, testToken, "", nil).Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	// A 0.1.0 client reads `state`, sees processing, and keeps polling. A
	// newer one reads the rest and starts playing.
	if view["state"] != "processing" {
		t.Errorf("state is %v; an early address must not look like completion", view["state"])
	}
	if view["address"] != "/media/out/"+assetID+"/master.m3u8" || view["growing"] != true || view["progress"] != 0.3 {
		t.Errorf("while encoding: address=%v growing=%v progress=%v", view["address"], view["growing"], view["progress"])
	}
	// The job fields the older view had are still beside them.
	if view["job_state"] == nil || view["job_id"] == nil {
		t.Errorf("the job fields are gone: %v", view)
	}

	// And in the list.
	var list struct {
		Assets []map[string]any `json:"assets"`
	}
	if err := json.NewDecoder(do(t, ts, http.MethodGet, "/v1/assets", testToken, "", nil).Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Assets) != 1 || list.Assets[0]["growing"] != true {
		t.Errorf("the list shows %v", list.Assets)
	}
}

func TestLiveRoutesSayHowToTurnLiveOn(t *testing.T) {
	// The server every older test builds: no live manager.
	ts, _, _ := newServer(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/live"},
		{http.MethodGet, "/v1/live"},
		{http.MethodGet, "/v1/live/01ANY"},
		{http.MethodPost, "/v1/live/01ANY/end"},
	} {
		// Behind the token like everything else, on or off.
		if resp := do(t, ts, c.method, c.path, "", "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d", c.method, c.path, resp.StatusCode)
		}
		resp := do(t, ts, c.method, c.path, testToken, "", nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s on a node with no RTMP listener: %d, want 503", c.method, c.path, resp.StatusCode)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "--rtmp-bind") {
			t.Errorf("%s %s does not say what would turn it on: %s", c.method, c.path, body)
		}
	}
}

func newLiveServer(t *testing.T) *httptest.Server {
	t.Helper()
	pool := testdb.Pool(t)
	objects, err := store.NewFS(t.TempDir(), "https://cdn.example", "/media")
	if err != nil {
		t.Fatal(err)
	}
	repo, jobs := assets.Open(pool), queue.Open(pool)
	streams, err := live.New(live.Config{
		Repo:      live.OpenRepo(pool),
		Assets:    repo,
		Store:     objects,
		WorkDir:   t.TempDir(),
		IngestURL: "rtmp://ingest.example/live",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{
		Assets: repo, Queue: jobs, Store: objects,
		Ingest: &ingest.Ingestor{Assets: repo, Queue: jobs, Store: objects},
		Live:   streams, Token: testToken, MediaPrefix: "/media", Version: "test",
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestALiveStreamIsCreatedReadAndEndedOverTheAPI(t *testing.T) {
	ts := newLiveServer(t)

	resp := do(t, ts, http.MethodPost, "/v1/live", testToken, "", nil)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	// The body carries a credential that will never be shown again.
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("the response carrying the key has Cache-Control %q", got)
	}
	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	streamID, _ := created["id"].(string)
	key, _ := created["stream_key"].(string)
	assetID, _ := created["asset_id"].(string)
	if streamID == "" || assetID == "" || created["state"] != "idle" {
		t.Fatalf("created: %v", created)
	}
	if !strings.HasPrefix(key, streamID+"_") || len(key) < len(streamID)+40 {
		t.Errorf("the stream key is %q", key)
	}
	if created["ingest_url"] != "rtmp://ingest.example/live" {
		t.Errorf("ingest_url is %v", created["ingest_url"])
	}
	// The playback address, through whatever fronts the node, before anyone
	// has broadcast.
	if created["address"] != "https://cdn.example/media/out/"+assetID+"/master.m3u8" {
		t.Errorf("address is %v", created["address"])
	}
	if got := resp.Header.Get("Location"); got != "/v1/live/"+streamID {
		t.Errorf("Location is %q", got)
	}
	// The hash never leaves the process, in any spelling.
	raw, _ := json.Marshal(created)
	if strings.Contains(string(raw), "key_hash") || strings.Contains(string(raw), "KeyHash") || strings.Contains(string(raw), live.HashKey(key)) {
		t.Errorf("the key's hash is in the response: %s", raw)
	}

	// Reading it back, alone or in the list, never shows the key.
	for _, path := range []string{"/v1/live/" + streamID, "/v1/live"} {
		body, _ := io.ReadAll(do(t, ts, http.MethodGet, path, testToken, "", nil).Body)
		if strings.Contains(string(body), "stream_key") || strings.Contains(string(body), strings.TrimPrefix(key, streamID+"_")) {
			t.Errorf("GET %s shows the key: %s", path, body)
		}
		if !strings.Contains(string(body), streamID) || !strings.Contains(string(body), `"state":"idle"`) {
			t.Errorf("GET %s: %s", path, body)
		}
	}

	// Ending an idle stream revokes its key.
	var ended map[string]any
	resp = do(t, ts, http.MethodPost, "/v1/live/"+streamID+"/end", testToken, "", nil)
	if err := json.NewDecoder(resp.Body).Decode(&ended); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("end: %d %v", resp.StatusCode, err)
	}
	if ended["state"] != "ended" || ended["ended_at"] == nil {
		t.Errorf("ended: %v", ended)
	}

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/live/01NOSUCHSTREAM"},
		{http.MethodPost, "/v1/live/01NOSUCHSTREAM/end"},
	} {
		if resp := do(t, ts, c.method, c.path, testToken, "", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c.method, c.path, resp.StatusCode)
		}
	}
	// A wrong token is refused before anything is created.
	if resp := do(t, ts, http.MethodPost, "/v1/live", "not-the-token", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("create with a wrong token: %d", resp.StatusCode)
	}
}
