package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The webhook surface is the one endpoint whose call rate the node does not
// control, so these tests are about what happens when the caller is hostile
// rather than about the happy path (covered in ingress_test.go).

func signParts(t *testing.T, secret string, parts ...string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(mac.Sum(nil))
}

// declareRoute registers a route through the real HTTP surface, so the tests
// exercise the same path an operator does.
func declareRoute(t *testing.T, h http.Handler, route IngressRoute) {
	t.Helper()
	body, _ := json.Marshal(route)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingress", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("declare route: %d %s", rec.Code, rec.Body.String())
	}
}

func deliver(t *testing.T, h http.Handler, name, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+name, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// THE regression. HMAC proves a body was signed by someone holding the secret;
// it says nothing about when. Before the replay table, a delivery captured off
// the wire could be posted back forever and every copy verified, opened a
// session and re-ran the graph — on an endpoint that is public by design.
func TestCapturedDeliveryCannotBeReplayed(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{Name: "stripe", Graph: "hooks", SecretEnv: "HOOK_SECRET"})

	body := `{"event":"charge.succeeded","amount":9900}`
	hdr := map[string]string{"X-Signature-256": signParts(t, hookSecret, body)}

	first := deliver(t, h, "stripe", body, hdr)
	if first.Code != 202 {
		t.Fatalf("first delivery: %d %s", first.Code, first.Body.String())
	}

	// Byte-identical replay, same valid signature.
	for i := 0; i < 3; i++ {
		again := deliver(t, h, "stripe", body, hdr)
		if again.Code == 202 {
			t.Fatalf("replay %d was accepted and opened a session", i+1)
		}
		if again.Code != 200 {
			t.Fatalf("replay %d answered %d; want 200 (already processed)", i+1, again.Code)
		}
		if !strings.Contains(again.Body.String(), "already processed") {
			t.Errorf("replay %d body = %s", i+1, again.Body.String())
		}
	}
}

// A retry is answered 200 rather than 4xx on purpose: a sender retrying under
// at-least-once is behaving correctly, and an error makes it retry harder.
func TestReplayIsAnsweredSuccessfullyNotAsAnError(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{Name: "gh", Graph: "hooks", SecretEnv: "HOOK_SECRET"})

	body := `{"ref":"refs/heads/main"}`
	hdr := map[string]string{"X-Signature-256": signParts(t, hookSecret, body)}

	_ = deliver(t, h, "gh", body, hdr)
	retry := deliver(t, h, "gh", body, hdr)
	if retry.Code < 200 || retry.Code >= 300 {
		t.Fatalf("a legitimate retry got %d; senders escalate on non-2xx", retry.Code)
	}
}

// Different deliveries must still get through — a replay check that refuses
// everything would be safe and useless.
func TestDistinctDeliveriesAreAllAccepted(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{Name: "gh", Graph: "hooks", SecretEnv: "HOOK_SECRET"})

	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"n":%d}`, i)
		rec := deliver(t, h, "gh", body, map[string]string{
			"X-Signature-256": signParts(t, hookSecret, body),
		})
		if rec.Code != 202 {
			t.Fatalf("delivery %d refused: %d %s", i, rec.Code, rec.Body.String())
		}
	}
}

// --- the signed timestamp ----------------------------------------------------

func TestSignedTimestampBindsTheDeliveryToAMoment(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{
		Name: "stripe", Graph: "hooks", SecretEnv: "HOOK_SECRET",
		TimestampHeader: "X-Timestamp", ToleranceSeconds: 300,
	})

	body := `{"event":"ok"}`
	now := fmt.Sprint(time.Now().Unix())

	fresh := deliver(t, h, "stripe", body, map[string]string{
		"X-Timestamp":     now,
		"X-Signature-256": signParts(t, hookSecret, now, ".", body),
	})
	if fresh.Code != 202 {
		t.Fatalf("a fresh signed delivery was refused: %d %s", fresh.Code, fresh.Body.String())
	}
}

func TestStaleTimestampIsRefused(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{
		Name: "stripe", Graph: "hooks", SecretEnv: "HOOK_SECRET",
		TimestampHeader: "X-Timestamp", ToleranceSeconds: 60,
	})

	body := `{"event":"old"}`
	old := fmt.Sprint(time.Now().Add(-10 * time.Minute).Unix())

	rec := deliver(t, h, "stripe", body, map[string]string{
		"X-Timestamp":     old,
		"X-Signature-256": signParts(t, hookSecret, old, ".", body),
	})
	if rec.Code != 401 {
		t.Fatalf("a correctly signed but stale delivery got %d; want 401", rec.Code)
	}
}

// A timestamp far in the future is a sender whose clock bounds nothing.
func TestFutureTimestampIsRefused(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{
		Name: "stripe", Graph: "hooks", SecretEnv: "HOOK_SECRET",
		TimestampHeader: "X-Timestamp", ToleranceSeconds: 60,
	})

	body := `{"event":"future"}`
	ahead := fmt.Sprint(time.Now().Add(time.Hour).Unix())

	rec := deliver(t, h, "stripe", body, map[string]string{
		"X-Timestamp":     ahead,
		"X-Signature-256": signParts(t, hookSecret, ahead, ".", body),
	})
	if rec.Code != 401 {
		t.Fatalf("a delivery an hour in the future got %d; want 401", rec.Code)
	}
}

// The timestamp has to be *inside* the signature. If it were merely checked
// alongside it, an attacker could replay an old body with a fresh timestamp.
func TestTimestampIsCoveredByTheSignature(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{
		Name: "stripe", Graph: "hooks", SecretEnv: "HOOK_SECRET",
		TimestampHeader: "X-Timestamp", ToleranceSeconds: 300,
	})

	body := `{"event":"ok"}`
	now := fmt.Sprint(time.Now().Unix())

	rec := deliver(t, h, "stripe", body, map[string]string{
		"X-Timestamp": now,
		// Signature over the body alone — what a pre-timestamp capture holds.
		"X-Signature-256": signParts(t, hookSecret, body),
	})
	if rec.Code != 401 {
		t.Fatalf("a signature that did not cover the timestamp was accepted (%d)", rec.Code)
	}
}

func TestMissingTimestampWhenRequiredIsRefused(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{
		Name: "stripe", Graph: "hooks", SecretEnv: "HOOK_SECRET",
		TimestampHeader: "X-Timestamp",
	})

	body := `{"event":"ok"}`
	rec := deliver(t, h, "stripe", body, map[string]string{
		"X-Signature-256": signParts(t, hookSecret, body),
	})
	if rec.Code != 401 {
		t.Fatalf("a delivery missing its required timestamp got %d; want 401", rec.Code)
	}
}

// --- signatures --------------------------------------------------------------

func TestForgedSignatureIsRefused(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{Name: "gh", Graph: "hooks", SecretEnv: "HOOK_SECRET"})

	body := `{"event":"forged"}`
	for name, sig := range map[string]string{
		"wrong secret": signParts(t, "not-the-secret", body),
		"empty":        "",
		"garbage":      "deadbeef",
		"other body":   signParts(t, hookSecret, `{"event":"different"}`),
	} {
		t.Run(name, func(t *testing.T) {
			rec := deliver(t, h, "gh", body, map[string]string{"X-Signature-256": sig})
			if rec.Code != 401 {
				t.Fatalf("%s signature got %d; want 401", name, rec.Code)
			}
		})
	}
}

// --- rate limiting -----------------------------------------------------------

func TestRouteRateLimitCapsDeliveries(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{
		Name: "gh", Graph: "hooks", SecretEnv: "HOOK_SECRET", RatePerMinute: 3,
	})

	var limited bool
	for i := 0; i < 10; i++ {
		body := fmt.Sprintf(`{"n":%d}`, i)
		rec := deliver(t, h, "gh", body, map[string]string{
			"X-Signature-256": signParts(t, hookSecret, body),
		})
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a 429 without Retry-After leaves the sender guessing")
			}
			break
		}
	}
	if !limited {
		t.Fatal("ten distinct deliveries against a 3/min limit were all accepted")
	}
}

// The limit is checked before the signature so a flood costs the node a map
// lookup rather than an HMAC per request.
func TestRateLimitAppliesBeforeSignatureWork(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	g, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{
		Name: "gh", Graph: "hooks", SecretEnv: "HOOK_SECRET", RatePerMinute: 1,
	})

	_ = deliver(t, h, "gh", `{"n":0}`, map[string]string{
		"X-Signature-256": signParts(t, hookSecret, `{"n":0}`)})

	// Unsigned garbage: it must be refused for the rate, not for the signature.
	rec := deliver(t, h, "gh", `{"n":1}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d; the rate limit should reject before the signature check", rec.Code)
	}
	_ = g
}

// --- session pressure --------------------------------------------------------

// A public endpoint that opens a session per delivery is a session generator
// unless something says no.
func TestSessionCapRefusesRatherThanExhausting(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	g, h, _ := ingressFixture(t)
	g.Mgr.SetMaxSessions(2)
	declareRoute(t, h, IngressRoute{
		Name: "gh", Graph: "hooks", SecretEnv: "HOOK_SECRET", RatePerMinute: 1000,
	})

	var refused bool
	for i := 0; i < 6; i++ {
		body := fmt.Sprintf(`{"n":%d}`, i)
		rec := deliver(t, h, "gh", body, map[string]string{
			"X-Signature-256": signParts(t, hookSecret, body),
		})
		if rec.Code == 503 {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("the node kept opening sessions past its cap instead of refusing")
	}
	if live := g.Mgr.LiveSessions(); live > 2 {
		t.Errorf("%d live sessions against a cap of 2", live)
	}
}

// --- the route cache ---------------------------------------------------------

// A revoked inbound URL that keeps working is a security problem, so the cache
// is invalidated on the write path rather than left to expire.
func TestRevokedRouteStopsWorkingImmediately(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)
	declareRoute(t, h, IngressRoute{Name: "gh", Graph: "hooks", SecretEnv: "HOOK_SECRET"})

	body := `{"n":1}`
	if rec := deliver(t, h, "gh", body, map[string]string{
		"X-Signature-256": signParts(t, hookSecret, body)}); rec.Code != 202 {
		t.Fatalf("setup delivery failed: %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/ingress/gh", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("revoke failed: %d %s", rec.Code, rec.Body.String())
	}

	body2 := `{"n":2}`
	after := deliver(t, h, "gh", body2, map[string]string{
		"X-Signature-256": signParts(t, hookSecret, body2)})
	if after.Code != 404 {
		t.Fatalf("a revoked route still answered %d; want 404", after.Code)
	}
}

// A route declared after the cache was populated must be visible at once.
func TestNewRouteIsVisibleImmediately(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)

	// Populate the cache with a miss.
	if rec := deliver(t, h, "later", `{}`, nil); rec.Code != 404 {
		t.Fatalf("want 404 for an undeclared route, got %d", rec.Code)
	}
	declareRoute(t, h, IngressRoute{Name: "later", Graph: "hooks", SecretEnv: "HOOK_SECRET"})

	body := `{"n":1}`
	rec := deliver(t, h, "later", body, map[string]string{
		"X-Signature-256": signParts(t, hookSecret, body)})
	if rec.Code != 202 {
		t.Fatalf("a freshly declared route answered %d; the cache went stale", rec.Code)
	}
}

// --- the store primitive -----------------------------------------------------

func TestSeenIngressDeliveryIsPerRoute(t *testing.T) {
	g, _, _ := ingressFixture(t)

	seen, err := g.St.SeenIngressDelivery("a", "digest-1", time.Minute)
	if err != nil || seen {
		t.Fatalf("first sighting: seen=%v err=%v", seen, err)
	}
	seen, err = g.St.SeenIngressDelivery("a", "digest-1", time.Minute)
	if err != nil || !seen {
		t.Fatalf("second sighting on the same route: seen=%v err=%v", seen, err)
	}
	// A different route with the same digest is different work.
	seen, err = g.St.SeenIngressDelivery("b", "digest-1", time.Minute)
	if err != nil || seen {
		t.Fatalf("same digest on another route was treated as a replay: seen=%v err=%v", seen, err)
	}
}

// Outside the window a digest is forgotten, which is what keeps the table
// proportional to traffic in the window rather than to traffic ever.
func TestSeenIngressDeliveryForgetsPastTheWindow(t *testing.T) {
	g, _, _ := ingressFixture(t)

	if _, err := g.St.SeenIngressDelivery("a", "digest-1", time.Minute); err != nil {
		t.Fatalf("first: %v", err)
	}
	// A zero window makes every existing row older than the cutoff.
	seen, err := g.St.SeenIngressDelivery("a", "digest-1", 0)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if seen {
		t.Error("a digest outside the window was still remembered")
	}
}
