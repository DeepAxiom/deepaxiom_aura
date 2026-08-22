package gateway

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The claim under test: a skill credential is narrow in the ways that matter and
// in no way merely nominal. Every test below is one way of trying to use a
// skill token as the operator's, and each has to be refused.

// fakeTokens is a resolver over a literal map, so these tests exercise the
// authorization decision rather than SQLite.
type fakeTokens map[string]TokenRow

func (f fakeTokens) TokenByHash(hash string) (TokenRow, bool, error) {
	row, ok := f[hash]
	return row, ok, nil
}

const (
	operatorToken = "operator-token-value"
	skillToken    = "skill-token-value"
)

func testAuth() *Auth {
	return &Auth{
		Token: operatorToken,
		Tokens: fakeTokens{
			HashToken(skillToken): {
				ID: "tok-1", Scope: string(ScopeSkill),
				Capability: "motor.erp.write", Label: "erp writer",
			},
			HashToken("revoked-token"): {
				ID: "tok-2", Scope: string(ScopeSkill),
				Capability: "motor.erp.write", Revoked: 1700000000000,
			},
		},
	}
}

// probe runs one request through the middleware and reports the status and the
// principal the handler saw.
func probe(t *testing.T, a *Auth, method, path, token string) (int, Principal) {
	t.Helper()
	var seen Principal
	h := a.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = PrincipalOf(r)
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, seen
}

func TestOperatorTokenKeepsFullAuthority(t *testing.T) {
	a := testAuth()
	for _, path := range []string{"/v1/graphs", "/v1/ledger", "/v1/operators", "/ws/skill"} {
		code, p := probe(t, a, http.MethodGet, path, operatorToken)
		if code != http.StatusOK {
			t.Errorf("operator refused %s: %d", path, code)
		}
		if !p.IsOperator() {
			t.Errorf("operator token resolved to scope %q on %s", p.Scope, path)
		}
	}
}

// The point of the whole feature: the routes that let a caller change what this
// node will do are closed to a skill.
func TestSkillTokenCannotReachTheControlSurface(t *testing.T) {
	a := testAuth()
	forbidden := []struct{ method, path string }{
		{http.MethodPost, "/v1/graphs"},      // register a graph — waive its own gate
		{http.MethodGet, "/v1/ledger"},       // read every effect the node sealed
		{http.MethodPost, "/v1/operators"},   // enrol an approver
		{http.MethodPost, "/v1/secrets"},     // store a secret
		{http.MethodGet, "/v1/sessions"},     // read other sessions
		{http.MethodPost, "/v1/skills/wasm"}, // host arbitrary code
	}
	for _, f := range forbidden {
		code, _ := probe(t, a, f.method, f.path, skillToken)
		if code != http.StatusForbidden {
			t.Errorf("%s %s with a skill token: got %d, want 403", f.method, f.path, code)
		}
	}
}

// ...and the three things a skill genuinely needs still work, or the feature
// would have been implemented by breaking skills.
func TestSkillTokenReachesWhatASkillNeeds(t *testing.T) {
	a := testAuth()
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/ws/skill"},
		{http.MethodPost, "/v1/secrets/resolve"},
		{http.MethodGet, "/healthz"},
	}
	for _, f := range allowed {
		code, p := probe(t, a, f.method, f.path, skillToken)
		if code != http.StatusOK {
			t.Errorf("%s %s with a skill token: got %d, want 200", f.method, f.path, code)
		}
		if p.Scope != ScopeSkill || p.Capability != "motor.erp.write" {
			t.Errorf("%s resolved to %+v; want a skill principal carrying its capability", f.path, p)
		}
	}
}

// A new route must be closed to skills until someone widens the table on
// purpose. If this ever fails, the model stopped being deny-by-default.
func TestUnknownRouteIsClosedToSkillsByDefault(t *testing.T) {
	a := testAuth()
	code, _ := probe(t, a, http.MethodGet, "/v1/some-route-added-next-year", skillToken)
	if code != http.StatusForbidden {
		t.Fatalf("a route nobody has considered was open to a skill token: %d", code)
	}
}

func TestRevokedTokenIsRefused(t *testing.T) {
	a := testAuth()
	code, _ := probe(t, a, http.MethodGet, "/ws/skill", "revoked-token")
	if code != http.StatusUnauthorized {
		t.Fatalf("a revoked token got %d, want 401", code)
	}
}

func TestUnknownTokenIsRefused(t *testing.T) {
	a := testAuth()
	code, _ := probe(t, a, http.MethodGet, "/ws/skill", "not-a-token-anyone-issued")
	if code != http.StatusUnauthorized {
		t.Fatalf("an invented token got %d, want 401", code)
	}
}

// A scope this binary does not know must not be treated as the permissive one.
// An older node reading a row written by a newer one is the case that matters.
func TestUnknownScopeIsRefusedRatherThanGuessed(t *testing.T) {
	a := &Auth{Token: operatorToken, Tokens: fakeTokens{
		HashToken("future"): {ID: "tok-9", Scope: "some-future-scope"},
	}}
	if code, _ := probe(t, a, http.MethodGet, "/v1/graphs", "future"); code != http.StatusUnauthorized {
		t.Fatalf("an uninterpretable scope got %d, want 401", code)
	}
}

// An open path serves callers holding nothing, so it must not require a
// credential — and must not promote one either. A skill token presented to a
// public route stays a skill token, and no credential at all is nobody.
func TestAnOpenPathDoesNotLaunderACredential(t *testing.T) {
	a := testAuth()

	if code, p := probe(t, a, http.MethodGet, "/healthz", skillToken); code != http.StatusOK ||
		p.Scope != ScopeSkill {
		t.Errorf("a skill token on an open path became %+v (code %d); it must stay a skill", p, code)
	}
	if code, p := probe(t, a, http.MethodGet, "/healthz", ""); code != http.StatusOK || p.IsOperator() {
		t.Errorf("an anonymous request on an open path became %+v; it must be nobody", p)
	}
}

// --- the binding that makes the token narrow ---------------------------------

func TestMayRegisterBindsToTheIssuedCapability(t *testing.T) {
	skill := Principal{Scope: ScopeSkill, Capability: "motor.erp.write"}

	if !skill.MayRegister("motor.erp.write") {
		t.Error("a skill token cannot register as the capability it was issued for")
	}
	// The attack this exists to stop: register as something the policy allows
	// outright, then act without a gate.
	if skill.MayRegister("motor.tts.speak") {
		t.Error("a skill token registered as a capability it was not issued for")
	}
	// A prefix is not a match. Issuing `motor.erp.read` must not confer
	// `motor.erp.delete`.
	if skill.MayRegister("motor.erp.write.extra") {
		t.Error("a capability prefix was accepted as a match")
	}
	if skill.MayRegister("") {
		t.Error("an empty capability was accepted")
	}
}

func TestOperatorMayRegisterAnything(t *testing.T) {
	op := Principal{Scope: ScopeOperator}
	for _, c := range []string{"motor.erp.write", "cognitive.llm.chat", "logical.echo"} {
		if !op.MayRegister(c) {
			t.Errorf("the operator was refused registration of %q", c)
		}
	}
}

// A principal that never went through authentication must be nobody, not the
// operator. This is the failure mode that turns a missing middleware into a
// full bypass.
func TestZeroPrincipalIsRefusedEverywhere(t *testing.T) {
	var nobody Principal
	if nobody.IsOperator() {
		t.Fatal("the zero principal is the operator")
	}
	if nobody.Allows(http.MethodGet, "/healthz") {
		t.Error("the zero principal was allowed a request")
	}
	if nobody.MayRegister("logical.echo") {
		t.Error("the zero principal was allowed to register")
	}
}

func TestHashTokenIsStableAndPrefixed(t *testing.T) {
	h := HashToken("abc")
	if h != HashToken(" abc ") {
		t.Error("surrounding whitespace changed the hash; a copied token would not resolve")
	}
	if !strings.HasPrefix(h, "sha256:") {
		t.Errorf("hash %q is not rendered like every other content hash in this codebase", h)
	}
	if strings.Contains(h, "abc") {
		t.Error("the hash contains the token")
	}
}

// `/metrics` was served without a token the moment it existed, because the
// asset rule asked "does this path have one segment" rather than "is this a
// route". Every future single-segment route would have been public the same way.
func TestOperationalRoutesAreNotMistakenForAssets(t *testing.T) {
	// Closed: they are routes, and metrics describes what the node is doing.
	for _, p := range []string{"/metrics"} {
		if openPath(p, false, registeredPathsForTest(t)) {
			t.Errorf("%s is served without a credential", p)
		}
	}
	// Open: a probe holds no credential, and refusing readiness keeps a healthy
	// node out of rotation forever.
	for _, p := range []string{"/healthz", "/readyz"} {
		if !openPath(p, false, registeredPathsForTest(t)) {
			t.Errorf("%s needs a credential; an orchestrator has none", p)
		}
	}
	// Still open: a browser must load the app shell before it can present a
	// token, so a client-side route is not a route this node serves.
	if !openPath("/sessions", false, registeredPathsForTest(t)) {
		t.Error("a single-page-app route was refused; the shell could never load")
	}
}

// The whole point of deriving the closed set from the mux: a route added later
// is closed because it was added, not because someone remembered.
func TestARouteInTheTableIsClosedWithoutBeingListedTwice(t *testing.T) {
	future := map[string]bool{"/some-route-added-next-year": true}
	if openPath("/some-route-added-next-year", false, future) {
		t.Fatal("a registered route was treated as a public asset")
	}
	if !openPath("/some-route-added-next-year", false, nil) {
		t.Fatal("fixture check: without the table the path looks like an app route, " +
			"which is exactly why the table has to come from the mux")
	}
}

// routesFromSource is every path Handler() registers, read out of the source
// of gateway.go with the go/ast parser.
//
// Parsing the file is deliberate, and the alternative is why: reading the map
// back off the Gateway (g.Auth.APIRoutes) would check that map against itself,
// and pass no matter what it contains — which is exactly how the missing-route
// bug this file guards against would survive its own test. net/http.ServeMux
// exposes no way to enumerate what was registered, so the registrations
// themselves are the only independent source there is.
func routesFromSource(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "gateway.go", nil, 0)
	if err != nil {
		t.Fatalf("parse gateway.go: %v", err)
	}
	var paths []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "handle" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if _, rest, found := strings.Cut(pattern, " "); found {
			pattern = rest // patterns are "METHOD /path"
		}
		paths = append(paths, pattern)
		return true
	})
	if len(paths) < 50 {
		t.Fatalf("found only %d routes in gateway.go; the parser is not seeing the registrations", len(paths))
	}
	return paths
}

// registeredPathsForTest is the route set Handler() hands the authenticator.
func registeredPathsForTest(t *testing.T) map[string]bool {
	t.Helper()
	g := &Gateway{Auth: &Auth{}}
	_ = g.Handler()
	if len(g.Auth.APIRoutes) == 0 {
		t.Fatal("Handler() registered no routes")
	}
	return g.Auth.APIRoutes
}

// TestEveryRouteIsAuthenticated is the check that was missing.
//
// isUIAsset serves anything it does not recognise as a route without a bearer
// token — it has to, because a browser must load the app shell before it can
// present one. So every route this gateway serves must be something isUIAsset
// refuses, whether by prefix or by appearing in the route table it is given. A
// route that is neither is a control-plane endpoint open to the network,
// silently, from the commit that added it.
//
// The four deliberate exceptions are named rather than skipped, so adding a
// fifth is a visible edit here.
func TestEveryRouteIsAuthenticated(t *testing.T) {
	routes := registeredPathsForTest(t)
	openByDesign := map[string]bool{
		"/":             true, // the app shell
		"/healthz":      true, // liveness, for probes holding no credential
		"/readyz":       true, // readiness, same
		"/hooks/{name}": true, // authenticates with the sender's own HMAC
	}
	for _, path := range routesFromSource(t) {
		if openByDesign[path] {
			continue
		}
		if isUIAsset(path, routes) {
			t.Errorf("route %q is treated as a UI asset, so it is served without a token", path)
		}
		if openPath(path, false, routes) {
			t.Errorf("route %q is reachable with no credential on a default node", path)
		}
	}
}
