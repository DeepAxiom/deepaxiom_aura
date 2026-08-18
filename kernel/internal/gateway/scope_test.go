package gateway

import (
	"net/http"
	"net/http/httptest"
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
