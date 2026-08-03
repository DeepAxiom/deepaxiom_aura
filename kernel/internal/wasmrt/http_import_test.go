package wasmrt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// A second, real compiled guest (see testdata/httpguest) — this file's job
// is proving env.http_fetch behaves correctly end to end, so it drives the
// actual ABI a skill author would use, not a stand-in for it.
var (
	httpGuestOnce  sync.Once
	httpGuestBytes []byte
	httpGuestErr   error
)

func buildHTTPGuest(t *testing.T) []byte {
	t.Helper()
	httpGuestOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "aura-wasmrt-test-httpguest")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			httpGuestErr = err
			return
		}
		out := filepath.Join(dir, "httpguest.wasm")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/httpguest")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if raw, err := cmd.CombinedOutput(); err != nil {
			httpGuestErr = err
			t.Logf("go build (GOOS=wasip1 GOARCH=wasm) failed:\n%s", raw)
			return
		}
		httpGuestBytes, httpGuestErr = os.ReadFile(out)
	})
	if httpGuestErr != nil {
		t.Skipf("cannot build the wasip1 http test guest in this environment: %v", httpGuestErr)
	}
	return httpGuestBytes
}

func testHTTPModule(t *testing.T) *Module {
	t.Helper()
	ctx := context.Background()
	rt, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	mod, err := rt.Compile(ctx, buildHTTPGuest(t))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return mod
}

// fetchGuestResponse is what testdata/httpguest reports on stdout.
type fetchGuestResponse struct {
	OK   bool   `json:"ok"`
	N    int    `json:"n"`
	Text string `json:"text"`
	Code int32  `json:"code"`
}

func invokeFetch(t *testing.T, mod *Module, perm Permission, method, targetURL string) fetchGuestResponse {
	t.Helper()
	req, _ := json.Marshal(map[string]string{"method": method, "url": targetURL})
	stdout, stderr, exitCode, err := mod.Invoke(context.Background(), req, perm)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("guest exited %d: stderr=%s", exitCode, stderr)
	}
	var resp fetchGuestResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		t.Fatalf("unmarshal guest stdout %q: %v", stdout, err)
	}
	return resp
}

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Hostname()
}

// --- happy path ----------------------------------------------------------

func TestHTTPFetchHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello from the host"))
	}))
	defer srv.Close()

	mod := testHTTPModule(t)
	resp := invokeFetch(t, mod, Permission{HTTPDomains: []string{hostOf(t, srv.URL)}}, "GET", srv.URL)
	if !resp.OK || resp.Text != "hello from the host" {
		t.Fatalf("resp = %+v, want ok=true text=%q", resp, "hello from the host")
	}
}

// --- adversarial: denied by default ---------------------------------------

func TestHTTPFetchDeniedByDefault(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write([]byte("should never be seen"))
	}))
	defer srv.Close()

	mod := testHTTPModule(t)
	resp := invokeFetch(t, mod, Permission{}, "GET", srv.URL) // no grant at all
	if resp.OK || resp.Code != httpFetchDenied {
		t.Fatalf("resp = %+v, want ok=false code=%d (denied)", resp, httpFetchDenied)
	}
	if n := atomic.LoadInt32(&requests); n != 0 {
		t.Fatalf("the test server received %d request(s); a denied fetch must never reach the network", n)
	}
}

// --- adversarial: exact-host match, not substring/suffix containment -----

func TestHTTPFetchAllowlistIsExactNotSubstring(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
	}))
	defer srv.Close()
	realHost := hostOf(t, srv.URL) // e.g. "127.0.0.1"

	// A grant that is a genuine substring of the real host must not match —
	// proves the matcher is exact-equality, not strings.Contains in disguise.
	substring := realHost[1:]
	if substring == realHost || substring == "" {
		t.Fatalf("test setup: %q is not a usable substring of %q", substring, realHost)
	}

	mod := testHTTPModule(t)
	resp := invokeFetch(t, mod, Permission{HTTPDomains: []string{substring}}, "GET", srv.URL)
	if resp.OK || resp.Code != httpFetchDenied {
		t.Fatalf("resp = %+v, want denied — %q must not match granted domain %q",
			resp, realHost, substring)
	}
	if n := atomic.LoadInt32(&requests); n != 0 {
		t.Fatalf("the test server received %d request(s) despite no exact grant", n)
	}
}

// --- adversarial: a response larger than the guest's buffer is truncated,
// --- not silently dropped or overflowed past the boundary -----------------

func TestHTTPFetchTruncatesAnOversizedResponse(t *testing.T) {
	const guestBufCap = 65536
	big := strings.Repeat("x", guestBufCap+1000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()

	mod := testHTTPModule(t)
	resp := invokeFetch(t, mod, Permission{HTTPDomains: []string{hostOf(t, srv.URL)}}, "GET", srv.URL)
	if !resp.OK {
		t.Fatalf("resp = %+v, want ok=true (truncated, not refused)", resp)
	}
	if resp.N != guestBufCap {
		t.Fatalf("n = %d, want exactly %d (the guest's buffer capacity)", resp.N, guestBufCap)
	}
	if resp.Text != big[:guestBufCap] {
		t.Fatal("truncated body does not match the prefix of what the server actually sent")
	}
}

// --- concurrency: permission scope is per-call, never shared/racy --------

// Two Invoke calls in flight at once against the same Module and the same
// server, one with a grant and one without, run many times — a permission
// field shared across calls instead of carried per-ctx would show up here
// as the denied call occasionally succeeding (or vice versa), not as a crash.
func TestHTTPFetchPermissionScopeDoesNotLeakAcrossConcurrentInvokes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("granted"))
	}))
	defer srv.Close()
	host := hostOf(t, srv.URL)

	mod := testHTTPModule(t)
	const rounds = 20
	var wg sync.WaitGroup
	errs := make(chan string, rounds*2)

	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp := invokeFetch(t, mod, Permission{HTTPDomains: []string{host}}, "GET", srv.URL)
			if !resp.OK || resp.Text != "granted" {
				errs <- "granted-side call was denied or wrong"
			}
		}()
		go func() {
			defer wg.Done()
			resp := invokeFetch(t, mod, Permission{}, "GET", srv.URL)
			if resp.OK || resp.Code != httpFetchDenied {
				errs <- "denied-side call was allowed through"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}
