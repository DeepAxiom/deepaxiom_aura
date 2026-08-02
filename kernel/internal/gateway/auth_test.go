package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

func request(t *testing.T, h http.Handler, method, target string, hdr http.Header) int {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

// --- the token ---------------------------------------------------------------

func TestTokenIsGeneratedOnceAndReused(t *testing.T) {
	dir := t.TempDir()

	first, created, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	if !created {
		t.Error("the first call should report that it created a token")
	}
	if len(first) < 32 {
		t.Errorf("token is only %d chars; too short to be a credential", len(first))
	}

	second, created, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateToken (second): %v", err)
	}
	if created {
		t.Error("the second call invented a new token; restarting a node must not lock out its clients")
	}
	if second != first {
		t.Errorf("token changed across calls: %q then %q", first, second)
	}
}

// A credential every account on the machine can read is not a credential.
// Skipped on Windows, whose permission model does not answer this question in
// the same terms.
func TestTokenFileIsNotWorldReadable(t *testing.T) {
	if os.Getenv("GOOS") == "windows" || filepath.Separator == '\\' {
		t.Skip("POSIX mode bits do not apply on this platform")
	}
	dir := t.TempDir()
	if _, _, err := LoadOrCreateToken(dir); err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, tokenFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("token file is %v; want no group or other access", perm)
	}
}

// --- what the token guards ---------------------------------------------------

func TestControlSurfaceRequiresTheToken(t *testing.T) {
	auth := &Auth{Token: "s3cret"}
	h := auth.Authenticate(okHandler())

	guarded := []string{
		"/v1/skills", // the catalog names every effect this node can produce
		"/v1/graphs",
		"/v1/sessions",
		"/v1/skills/config?id=x",
		"/mcp",
		"/.well-known/agent.json",
		"/ws/skill",
		"/v1/stream?graph=echo",
	}
	for _, path := range guarded {
		t.Run(path, func(t *testing.T) {
			if code := request(t, h, http.MethodGet, path, nil); code != http.StatusUnauthorized {
				t.Errorf("%s without a token returned %d; want 401", path, code)
			}
			if code := request(t, h, http.MethodGet, path, bearer("s3cret")); code != http.StatusOK {
				t.Errorf("%s with the right token returned %d; want 200", path, code)
			}
		})
	}
}

// Three paths are open, each for a reason. If this test starts failing because
// something was added to the list, the reason had better be written down.
func TestOpenPathsNeedNoToken(t *testing.T) {
	auth := &Auth{Token: "s3cret"}
	h := auth.Authenticate(okHandler())

	for _, path := range []string{
		"/healthz",         // an orchestrator has no credential
		"/hooks/stripe",    // authenticates with its own HMAC
		"/",                // the browser must load the page before it can present a token
		"/assets-index.js", // ...and its bundle
	} {
		if code := request(t, h, http.MethodGet, path, nil); code != http.StatusOK {
			t.Errorf("%s returned %d without a token; it is meant to be open", path, code)
		}
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	h := (&Auth{Token: "s3cret"}).Authenticate(okHandler())
	for _, hdr := range []http.Header{
		bearer("wrong"),
		bearer(""),
		{"Authorization": []string{"s3cret"}},         // no scheme
		{"Authorization": []string{"Basic s3cret"}},   // wrong scheme
		{"Authorization": []string{"Bearer s3cret "}}, // trailing space is a different string
	} {
		if code := request(t, h, http.MethodGet, "/v1/skills", hdr); code != http.StatusUnauthorized {
			t.Errorf("Authorization %q returned %d; want 401", hdr.Get("Authorization"), code)
		}
	}
}

// The browser WebSocket API cannot set headers, so the query form has to work
// — but only on the WS paths, or tokens would end up in HTTP access logs.
func TestQueryTokenWorksOnlyOnWebSocketPaths(t *testing.T) {
	h := (&Auth{Token: "s3cret"}).Authenticate(okHandler())

	for _, path := range []string{"/ws/skill?token=s3cret", "/v1/stream?graph=echo&token=s3cret"} {
		if code := request(t, h, http.MethodGet, path, nil); code != http.StatusOK {
			t.Errorf("%s returned %d; a browser has no other way to authenticate", path, code)
		}
	}
	if code := request(t, h, http.MethodGet, "/v1/skills?token=s3cret", nil); code != http.StatusUnauthorized {
		t.Errorf("the query form was accepted on an ordinary API path (%d); "+
			"that puts credentials in access logs", code)
	}
}

func TestNoTokenConfiguredLetsEverythingThrough(t *testing.T) {
	// --no-auth. It has to actually work, because the banner promises it does.
	h := (&Auth{}).Authenticate(okHandler())
	if code := request(t, h, http.MethodGet, "/v1/skills", nil); code != http.StatusOK {
		t.Errorf("an unauthenticated node refused a request: %d", code)
	}
}

func TestUnauthorizedResponseIsActionable(t *testing.T) {
	h := (&Auth{Token: "s3cret"}).Authenticate(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/skills", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q; a 401 should say how to authenticate", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, tokenFile) {
		t.Errorf("the 401 body does not say where to find the token: %s", body)
	}
}

// --- the origin check --------------------------------------------------------

// The token alone does not stop a malicious page: a browser attaches
// credentials to cross-site WebSocket handshakes on its own. This is the check
// that does, and it used to be `return true`.
func TestCrossOriginUpgradeIsRefused(t *testing.T) {
	auth := &Auth{Token: "s3cret"}
	req := httptest.NewRequest(http.MethodGet, "/ws/skill", nil)
	req.Host = "localhost:9080"
	req.Header.Set("Origin", "https://evil.example")

	if auth.CheckOrigin(req) {
		t.Fatal("an upgrade from an unrelated origin was accepted")
	}
}

func TestSameOriginUpgradeIsAccepted(t *testing.T) {
	auth := &Auth{Token: "s3cret"}
	req := httptest.NewRequest(http.MethodGet, "/ws/skill", nil)
	req.Host = "localhost:9080"
	req.Header.Set("Origin", "http://localhost:9080")

	if !auth.CheckOrigin(req) {
		t.Fatal("the node refused an upgrade from the page it serves itself")
	}
}

// Non-browser clients — the CLI, a skill, curl — send no Origin, and the token
// is the right check for them. Refusing them here would break every skill.
func TestMissingOriginIsAccepted(t *testing.T) {
	auth := &Auth{Token: "s3cret"}
	req := httptest.NewRequest(http.MethodGet, "/ws/skill", nil)
	if !auth.CheckOrigin(req) {
		t.Fatal("a non-browser client with no Origin was refused")
	}
}

func TestAllowlistedOriginIsAccepted(t *testing.T) {
	auth := &Auth{Token: "s3cret", AllowedOrigins: []string{"https://ops.example"}}
	req := httptest.NewRequest(http.MethodGet, "/ws/skill", nil)
	req.Host = "localhost:9080"
	req.Header.Set("Origin", "https://ops.example")

	if !auth.CheckOrigin(req) {
		t.Fatal("an explicitly allowed origin was refused")
	}
}

func TestMalformedOriginIsRefused(t *testing.T) {
	auth := &Auth{Token: "s3cret"}
	for _, origin := range []string{"://", "not a url", ""} {
		req := httptest.NewRequest(http.MethodGet, "/ws/skill", nil)
		req.Host = "localhost:9080"
		req.Header["Origin"] = []string{origin}
		if origin == "" {
			continue // absent is a different case, covered above
		}
		if auth.CheckOrigin(req) {
			t.Errorf("malformed Origin %q was accepted", origin)
		}
	}
}

// --- where the node binds ----------------------------------------------------

// The old default bound every interface, which meant a node started on a
// laptop in a café was served to the café.
func TestListenDefaultsToLoopback(t *testing.T) {
	addr, public, err := ResolveListen("", 9080)
	if err != nil {
		t.Fatalf("ResolveListen: %v", err)
	}
	if public {
		t.Error("the default binding reported itself as public")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("default addr = %q; want loopback", addr)
	}
}

func TestListenRecognisesPublicBindings(t *testing.T) {
	for _, tc := range []struct {
		listen     string
		wantPublic bool
	}{
		{"0.0.0.0", true},
		{"0.0.0.0:9090", true},
		{":9090", true}, // an empty host means every interface
		{"192.168.1.10", true},
		{"127.0.0.1", false},
		{"localhost", false},
		{"127.0.0.1:9999", false},
		{"[::1]:9999", false},
	} {
		_, public, err := ResolveListen(tc.listen, 9080)
		if err != nil {
			t.Errorf("ResolveListen(%q): %v", tc.listen, err)
			continue
		}
		if public != tc.wantPublic {
			t.Errorf("ResolveListen(%q) public = %v, want %v", tc.listen, public, tc.wantPublic)
		}
	}
}

func TestListenRejectsNonsense(t *testing.T) {
	if _, _, err := ResolveListen("1.2.3.4:5:6", 9080); err == nil {
		t.Error("a malformed --listen was accepted")
	}
}

func TestListenAppliesThePortToABareHost(t *testing.T) {
	addr, _, err := ResolveListen("0.0.0.0", 9091)
	if err != nil {
		t.Fatalf("ResolveListen: %v", err)
	}
	if addr != "0.0.0.0:9091" {
		t.Errorf("addr = %q; a bare host should take --port", addr)
	}
}

// --- end to end through the real gateway -------------------------------------

// The middleware wraps the whole mux, so a route added later is guarded by
// default. This checks the wiring rather than the middleware in isolation.
func TestGatewayHandlerIsGuardedWhenAuthIsSet(t *testing.T) {
	g, _ := testGateway(t)
	g.Auth = &Auth{Token: "s3cret"}
	h := g.Handler()

	if code := request(t, h, http.MethodGet, "/v1/skills", nil); code != http.StatusUnauthorized {
		t.Fatalf("the real gateway served the catalog unauthenticated (%d)", code)
	}
	if code := request(t, h, http.MethodGet, "/v1/skills", bearer("s3cret")); code != http.StatusOK {
		t.Fatalf("the real gateway refused a valid token (%d)", code)
	}
	if code := request(t, h, http.MethodGet, "/healthz", nil); code != http.StatusOK {
		t.Fatalf("healthz needs a token (%d); an orchestrator has none", code)
	}
}

func TestGatewayUpgraderUsesTheOriginCheck(t *testing.T) {
	g, _ := testGateway(t)
	g.Auth = &Auth{Token: "s3cret"}

	req := httptest.NewRequest(http.MethodGet, "/ws/skill", nil)
	req.Host = "localhost:9080"
	req.Header.Set("Origin", "https://evil.example")

	if g.upgrader().CheckOrigin(req) {
		t.Fatal("the gateway's upgrader still accepts any origin")
	}
}

func TestGatewayWithoutAuthAcceptsAnyOrigin(t *testing.T) {
	// --no-auth means no origin check either: there is nothing to protect.
	g, _ := testGateway(t)
	req := httptest.NewRequest(http.MethodGet, "/ws/skill", nil)
	req.Header.Set("Origin", "https://anything.example")

	if !g.upgrader().CheckOrigin(req) {
		t.Fatal("an unauthenticated node refused an upgrade")
	}
}

func ExampleResolveListen() {
	addr, public, _ := ResolveListen("", 9080)
	fmt.Println(addr, public)
	// Output: 127.0.0.1:9080 false
}
