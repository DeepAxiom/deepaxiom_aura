package hub

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aura/kernel/internal/signing"
)

// The hub is the registry server: it decides what a node is allowed to
// install. Every property below is a rule the marketplace's trust model
// depends on, and none of them had a test.

func testHub(t *testing.T) http.Handler {
	t.Helper()
	h, err := Open(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h.Handler()
}

func testManifest(id, version string) map[string]any {
	return map[string]any{
		"id": id, "version": version, "protocol": "1",
		"name": "Test skill", "description": "does a test thing",
		"capability": "logical.test", "type": "logical", "format": "source",
		"ports": map[string]any{
			"ingress": []map[string]any{{"name": "text_in", "schema": "std/text@1"}},
			"egress":  []map[string]any{{"name": "text_out", "schema": "std/text@1"}},
		},
	}
}

type published struct {
	manifest  map[string]any
	artifact  []byte
	pubkey    string
	signature string
}

func signPackage(t *testing.T, kp *signing.Keypair, manifest map[string]any, artifact []byte) published {
	t.Helper()
	mh, err := signing.CanonicalManifestHash(manifest)
	if err != nil {
		t.Fatalf("CanonicalManifestHash: %v", err)
	}
	payload := signing.Payload(mh, signing.ArtifactHash(artifact))
	return published{
		manifest: manifest, artifact: artifact,
		pubkey: kp.PublicB64(), signature: kp.Sign(payload),
	}
}

func post(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func get(t *testing.T, h http.Handler, path string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (p published) body() map[string]any {
	return map[string]any{
		"manifest": p.manifest, "pubkey": p.pubkey, "signature": p.signature,
		"artifact_b64": base64.StdEncoding.EncodeToString(p.artifact),
	}
}

func keypair(t *testing.T) *signing.Keypair {
	t.Helper()
	kp, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return kp
}

// --- the happy path ------------------------------------------------------------

func TestPublishThenFetch(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)
	pkg := signPackage(t, kp, testManifest("acme/logical/test", "1.0.0"), []byte("artifact bytes"))

	code, body := post(t, h, "/r1/packages", pkg.body())
	if code != 201 {
		t.Fatalf("publish: %d %v", code, body)
	}

	code, raw := get(t, h, "/r1/packages/acme/logical/test/1.0.0")
	if code != 200 {
		t.Fatalf("fetch: %d %s", code, raw)
	}
	code, artifact := get(t, h, "/r1/packages/acme/logical/test/1.0.0/artifact")
	if code != 200 {
		t.Fatalf("artifact: %d", code)
	}
	if string(artifact) != "artifact bytes" {
		t.Errorf("artifact came back as %q", artifact)
	}
}

func TestHealthIsServed(t *testing.T) {
	h := testHub(t)
	if code, _ := get(t, h, "/r1/health"); code != 200 {
		t.Fatalf("health: %d", code)
	}
}

// --- what the registry refuses --------------------------------------------------

// The registry is the enforcement point of the trust model, so a bad signature
// has to be refused *here* rather than left for each installing node to notice.
func TestUnsignedOrBadlySignedPublishIsRefused(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)
	manifest := testManifest("acme/logical/test", "1.0.0")
	artifact := []byte("artifact bytes")
	good := signPackage(t, kp, manifest, artifact)

	t.Run("no signature", func(t *testing.T) {
		body := good.body()
		delete(body, "signature")
		if code, _ := post(t, h, "/r1/packages", body); code != 422 {
			t.Fatalf("an unsigned publish returned %d; want 422", code)
		}
	})

	t.Run("no pubkey", func(t *testing.T) {
		body := good.body()
		delete(body, "pubkey")
		if code, _ := post(t, h, "/r1/packages", body); code != 422 {
			t.Fatalf("a publish with no key returned %d; want 422", code)
		}
	})

	t.Run("signature over other content", func(t *testing.T) {
		other := signPackage(t, kp, manifest, []byte("different artifact"))
		body := good.body()
		body["signature"] = other.signature
		if code, _ := post(t, h, "/r1/packages", body); code != 403 {
			t.Fatalf("a mismatched signature returned %d; want 403", code)
		}
	})

	t.Run("artifact swapped after signing", func(t *testing.T) {
		body := good.body()
		body["artifact_b64"] = base64.StdEncoding.EncodeToString([]byte("malware"))
		if code, _ := post(t, h, "/r1/packages", body); code != 403 {
			t.Fatalf("a swapped artifact returned %d; want 403", code)
		}
	})
}

func TestInvalidManifestIsRefused(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)

	bad := testManifest("not a valid id", "1.0.0")
	pkg := signPackage(t, kp, bad, []byte("x"))
	if code, body := post(t, h, "/r1/packages", pkg.body()); code != 422 {
		t.Fatalf("an invalid C1 manifest returned %d (%v); want 422", code, body)
	}
}

func TestMissingArtifactIsRefused(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)
	pkg := signPackage(t, kp, testManifest("acme/logical/test", "1.0.0"), []byte("x"))
	body := pkg.body()
	body["artifact_b64"] = ""

	if code, _ := post(t, h, "/r1/packages", body); code != 422 {
		t.Fatalf("a publish with no artifact returned %d; want 422", code)
	}
}

func TestMalformedJSONIsRefused(t *testing.T) {
	h := testHub(t)
	req := httptest.NewRequest(http.MethodPost, "/r1/packages", strings.NewReader("{nope"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("malformed json returned %d; want 400", rec.Code)
	}
}

// --- trust on first use ---------------------------------------------------------

// The first publish of an id binds the publisher key. A second publisher
// taking over that name is exactly the supply-chain attack this prevents.
func TestPackageIdIsBoundToItsFirstPublisher(t *testing.T) {
	h := testHub(t)
	first, second := keypair(t), keypair(t)

	pkg := signPackage(t, first, testManifest("acme/logical/test", "1.0.0"), []byte("original"))
	if code, body := post(t, h, "/r1/packages", pkg.body()); code != 201 {
		t.Fatalf("first publish: %d %v", code, body)
	}

	hijack := signPackage(t, second, testManifest("acme/logical/test", "2.0.0"), []byte("hijacked"))
	code, body := post(t, h, "/r1/packages", hijack.body())
	if code != 409 {
		t.Fatalf("a different publisher took over the id (%d); want 409", code)
	}
	if !strings.Contains(strings.ToLower(body["error"].(string)), "trust-on-first-use") {
		t.Errorf("the refusal does not explain itself: %v", body["error"])
	}
}

func TestSamePublisherCanShipNewVersions(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)

	for _, version := range []string{"1.0.0", "1.0.1", "2.0.0"} {
		pkg := signPackage(t, kp, testManifest("acme/logical/test", version), []byte("v"+version))
		if code, body := post(t, h, "/r1/packages", pkg.body()); code != 201 {
			t.Fatalf("publishing %s: %d %v", version, code, body)
		}
	}
}

// --- version immutability -------------------------------------------------------

// A published version that can change under a consumer is the other classic
// supply-chain problem, so republishing different content is refused...
func TestRepublishingAVersionWithNewContentIsRefused(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)

	original := signPackage(t, kp, testManifest("acme/logical/test", "1.0.0"), []byte("original"))
	if code, _ := post(t, h, "/r1/packages", original.body()); code != 201 {
		t.Fatal("setup publish failed")
	}

	changed := signPackage(t, kp, testManifest("acme/logical/test", "1.0.0"), []byte("changed"))
	code, body := post(t, h, "/r1/packages", changed.body())
	if code != 409 {
		t.Fatalf("republishing 1.0.0 with new content returned %d; want 409", code)
	}
	if !strings.Contains(strings.ToLower(body["error"].(string)), "immutable") {
		t.Errorf("the refusal does not explain itself: %v", body["error"])
	}
}

// ...while republishing byte-identical content is idempotent, so a retried
// publish in CI is not an error.
func TestRepublishingIdenticalContentIsIdempotent(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)
	pkg := signPackage(t, kp, testManifest("acme/logical/test", "1.0.0"), []byte("same"))

	if code, _ := post(t, h, "/r1/packages", pkg.body()); code != 201 {
		t.Fatal("first publish failed")
	}
	code, body := post(t, h, "/r1/packages", pkg.body())
	if code != 200 {
		t.Fatalf("an identical republish returned %d; want 200", code)
	}
	if body["status"] != "already published" {
		t.Errorf("status = %v", body["status"])
	}
}

// --- discovery -------------------------------------------------------------------

func TestPackagesAreDiscoverableByCapability(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)

	m := testManifest("acme/logical/ocr", "1.0.0")
	m["capability"] = "sensorial.ocr.read"
	m["type"] = "sensorial"
	pkg := signPackage(t, kp, m, []byte("ocr"))
	if code, body := post(t, h, "/r1/packages", pkg.body()); code != 201 {
		t.Fatalf("publish: %d %v", code, body)
	}

	code, raw := get(t, h, "/r1/packages?capability=sensorial.ocr.read")
	if code != 200 {
		t.Fatalf("discovery: %d", code)
	}
	if !strings.Contains(string(raw), "acme/logical/ocr") {
		t.Errorf("capability search did not find the package: %s", raw)
	}

	_, miss := get(t, h, "/r1/packages?capability=motor.nothing.here")
	if strings.Contains(string(miss), "acme/logical/ocr") {
		t.Errorf("capability search returned an unrelated package: %s", miss)
	}
}

func TestUnknownPackageIs404(t *testing.T) {
	h := testHub(t)
	if code, _ := get(t, h, "/r1/packages/no/such/pkg/1.0.0"); code != 404 {
		t.Fatalf("an unknown package returned %d; want 404", code)
	}
	if code, _ := get(t, h, "/r1/packages/no/such/pkg"); code != 404 {
		t.Fatalf("versions of an unknown package returned %d; want 404", code)
	}
}

// `@latest` has to resolve in semver order rather than string order —
// otherwise 1.10.0 sorts below 1.9.0 and an "upgrade" silently installs an
// older build. Publishing order is deliberately not version order here, so a
// resolver that just took the last row would fail this.
func TestLatestResolvesInSemverOrder(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)

	for _, version := range []string{"1.9.0", "1.10.0", "1.2.0"} {
		pkg := signPackage(t, kp, testManifest("acme/logical/test", version), []byte("v"+version))
		if code, _ := post(t, h, "/r1/packages", pkg.body()); code != 201 {
			t.Fatalf("publishing %s failed", version)
		}
	}

	code, raw := get(t, h, "/r1/packages/acme/logical/test/latest")
	if code != 200 {
		t.Fatalf("latest: %d %s", code, raw)
	}
	var pkg map[string]any
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("bad response: %s", raw)
	}
	if pkg["version"] != "1.10.0" {
		t.Errorf("latest resolved to %v; want 1.10.0 (semver order, not string order)", pkg["version"])
	}
}

// The versions listing returns every published version of an id.
func TestVersionsListsEverythingPublished(t *testing.T) {
	h := testHub(t)
	kp := keypair(t)

	for _, version := range []string{"1.0.0", "1.1.0"} {
		pkg := signPackage(t, kp, testManifest("acme/logical/test", version), []byte("v"+version))
		if code, _ := post(t, h, "/r1/packages", pkg.body()); code != 201 {
			t.Fatalf("publishing %s failed", version)
		}
	}

	code, raw := get(t, h, "/r1/packages/acme/logical/test")
	if code != 200 {
		t.Fatalf("versions: %d", code)
	}
	var out struct {
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("bad response: %s", raw)
	}
	if len(out.Versions) != 2 {
		t.Fatalf("listed %d versions; want 2", len(out.Versions))
	}
}

func TestSemverOrdering(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		less bool
	}{
		{"1.2.0", "1.10.0", true}, // the string-order trap
		{"1.9.0", "1.10.0", true},
		{"2.0.0", "1.99.99", false},
		{"1.0.0", "1.0.1", true},
		{"1.0.0", "1.0.0", false},
	} {
		if got := semverLess(tc.a, tc.b); got != tc.less {
			t.Errorf("semverLess(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.less)
		}
	}
}
