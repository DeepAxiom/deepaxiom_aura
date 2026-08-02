package gateway

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aura/kernel/internal/registry"
)

const hookSecret = "shhh-this-is-the-shared-secret"

func sign(t *testing.T, body string, encoding string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(hookSecret))
	mac.Write([]byte(body))
	if encoding == "base64" {
		return base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// ingressFixture registers a skill, a graph pointing at it, and returns a
// gateway ready to accept deliveries.
func ingressFixture(t *testing.T) (*Gateway, http.Handler, *[][]byte) {
	t.Helper()
	g, h := testGateway(t)

	delivered := &[][]byte{}
	m := registry.Manifest{
		ID: "acme/logical/hooked", Version: "1.0.0", Protocol: "1",
		Name: "hooked", Description: "receives webhook deliveries",
		Capability: "logical.hooked", Type: "logical", Format: "source",
	}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	g.Reg.Register("conn-hooked", &registry.Live{
		Manifest: m, Connected: time.Now(),
		Send: func(raw []byte, _ string) error { *delivered = append(*delivered, raw); return nil },
	})

	graph := map[string]any{
		"ir": "1", "graph_id": "hooks", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "h", "resolve": "logical.hooked"}},
		"edges": []map[string]any{{"from": "client.text_out", "to": "h.text_in"}},
	}
	if code, _ := do(t, h, "POST", "/v1/graphs", graph); code != 201 {
		t.Fatalf("register graph: %d", code)
	}
	return g, h, delivered
}

func postHook(t *testing.T, h http.Handler, name, body, signature string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/hooks/"+name, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("X-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A route with no secret and no explicit opt-out must be refused. Leaving a
// public endpoint unauthenticated has to be something you ask for, not
// something you get by forgetting a field.
func TestDeclareIngressRequiresASecretOrAnExplicitOptOut(t *testing.T) {
	_, h, _ := ingressFixture(t)

	code, body := do(t, h, "POST", "/v1/ingress",
		map[string]any{"name": "stripe", "graph": "hooks"})
	if code != 422 {
		t.Fatalf("status = %d, want 422 for a route with no secret", code)
	}
	if body["error"] == nil {
		t.Fatal("want an explanation of how to fix it")
	}

	// Naming a variable that is not set is equally refused: every delivery
	// would fail, and the operator should hear about it now.
	code, _ = do(t, h, "POST", "/v1/ingress", map[string]any{
		"name": "stripe", "graph": "hooks", "secret_env": "DEFINITELY_NOT_SET_ANYWHERE"})
	if code != 422 {
		t.Fatalf("status = %d, want 422 when the secret env var is empty", code)
	}
}

func TestDeclareIngressRejectsAnUnknownGraph(t *testing.T) {
	_, h, _ := ingressFixture(t)
	t.Setenv("HOOK_SECRET", hookSecret)

	code, _ := do(t, h, "POST", "/v1/ingress", map[string]any{
		"name": "stripe", "graph": "no-such-graph", "secret_env": "HOOK_SECRET"})
	if code != 422 {
		t.Fatalf("status = %d, want 422 — an unroutable webhook should fail at "+
			"declaration, not on the sender's first delivery", code)
	}
}

// The delivery path: a correctly signed body becomes an envelope in a session
// of the target graph.
func TestSignedDeliveryReachesTheGraph(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, delivered := ingressFixture(t)

	code, body := do(t, h, "POST", "/v1/ingress", map[string]any{
		"name": "stripe", "graph": "hooks", "secret_env": "HOOK_SECRET"})
	if code != 201 {
		t.Fatalf("declare: status = %d, body %v", code, body)
	}
	if body["url"] != "/hooks/stripe" {
		t.Fatalf("want the endpoint URL returned, got %v", body["url"])
	}

	payload := `{"event":"invoice.paid","amount":4200}`
	code, resp := postHook(t, h, "stripe", payload, sign(t, payload, "hex"))
	if code != 202 {
		t.Fatalf("delivery: status = %d, want 202", code)
	}
	if resp["session"] == nil {
		t.Fatal("want the session id back so the delivery can be explained later")
	}

	if len(*delivered) != 1 {
		t.Fatalf("skill received %d envelope(s), want 1", len(*delivered))
	}
	var env struct {
		Schema  string         `json:"schema"`
		Payload map[string]any `json:"payload"`
	}
	_ = json.Unmarshal((*delivered)[0], &env)
	if env.Payload["event"] != "invoice.paid" || env.Payload["amount"] != float64(4200) {
		t.Fatalf("the JSON body should reach the graph as-is, got %v", env.Payload)
	}
}

// The adversarial cases. A signature check that accepts any of these is worse
// than none, because it looks like protection.
func TestDeliveryIsRejectedWithoutAValidSignature(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, delivered := ingressFixture(t)

	if code, _ := do(t, h, "POST", "/v1/ingress", map[string]any{
		"name": "stripe", "graph": "hooks", "secret_env": "HOOK_SECRET"}); code != 201 {
		t.Fatalf("declare: %d", code)
	}

	payload := `{"event":"invoice.paid"}`
	valid := sign(t, payload, "hex")

	cases := []struct {
		name      string
		body      string
		signature string
	}{
		{"no signature at all", payload, ""},
		{"empty signature", payload, "   "},
		{"wrong signature", payload, hex.EncodeToString(make([]byte, 32))},
		{"signature from a different secret", payload,
			"0000000000000000000000000000000000000000000000000000000000000000"},
		{"truncated signature", payload, valid[:32]},
		// The one that matters most: a valid signature replayed over a
		// tampered body. This is the actual attack.
		{"valid signature, tampered body", `{"event":"invoice.paid","amount":999999}`, valid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(*delivered)
			code, _ := postHook(t, h, "stripe", tc.body, tc.signature)
			if code != 401 {
				t.Fatalf("status = %d, want 401", code)
			}
			if len(*delivered) != before {
				t.Fatal("a rejected delivery still reached the graph")
			}
		})
	}
}

func TestBase64SignatureEncoding(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, delivered := ingressFixture(t)

	if code, _ := do(t, h, "POST", "/v1/ingress", map[string]any{
		"name": "gh", "graph": "hooks", "secret_env": "HOOK_SECRET",
		"signature_encoding": "base64", "signature_prefix": "sha256="}); code != 201 {
		t.Fatalf("declare: %d", code)
	}

	payload := `{"action":"opened"}`
	if code, _ := postHook(t, h, "gh", payload, "sha256="+sign(t, payload, "base64")); code != 202 {
		t.Fatalf("status = %d, want 202 for a valid base64 signature with a prefix", code)
	}
	if len(*delivered) != 1 {
		t.Fatalf("want the delivery routed, got %d", len(*delivered))
	}
}

// Opting out is allowed, but only deliberately.
func TestUnsignedRouteAcceptsDeliveriesWhenAskedFor(t *testing.T) {
	_, h, delivered := ingressFixture(t)

	if code, _ := do(t, h, "POST", "/v1/ingress", map[string]any{
		"name": "open", "graph": "hooks", "unsigned": true}); code != 201 {
		t.Fatalf("declare: %d", code)
	}
	if code, _ := postHook(t, h, "open", `{"x":1}`, ""); code != 202 {
		t.Fatalf("status = %d, want 202 on a deliberately unsigned route", code)
	}
	if len(*delivered) != 1 {
		t.Fatalf("want the delivery routed, got %d", len(*delivered))
	}
}

// An inbound URL you cannot revoke is a security problem in itself.
func TestIngressRouteCanBeListedAndRevoked(t *testing.T) {
	t.Setenv("HOOK_SECRET", hookSecret)
	_, h, _ := ingressFixture(t)

	if code, _ := do(t, h, "POST", "/v1/ingress", map[string]any{
		"name": "stripe", "graph": "hooks", "secret_env": "HOOK_SECRET"}); code != 201 {
		t.Fatalf("declare: %d", code)
	}

	code, body := do(t, h, "GET", "/v1/ingress", nil)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	raw, _ := json.Marshal(body)
	if bytes.Contains(raw, []byte(hookSecret)) {
		t.Fatalf("the shared secret was served to the client:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("HOOK_SECRET")) {
		t.Fatal("want the env var NAME kept, so an operator can see where the secret comes from")
	}

	if code, _ := do(t, h, "DELETE", "/v1/ingress/stripe", nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := postHook(t, h, "stripe", `{}`, sign(t, `{}`, "hex")); code != 404 {
		t.Fatalf("status = %d, want 404 after the route was revoked", code)
	}
	if code, _ := do(t, h, "DELETE", "/v1/ingress/stripe", nil); code != 404 {
		t.Fatalf("second revoke: status = %d, want 404", code)
	}
}

// A non-JSON body still has to reach a graph that expects text.
func TestNonJSONBodyIsWrappedAsText(t *testing.T) {
	_, h, delivered := ingressFixture(t)

	if code, _ := do(t, h, "POST", "/v1/ingress", map[string]any{
		"name": "plain", "graph": "hooks", "unsigned": true}); code != 201 {
		t.Fatalf("declare: %d", code)
	}

	req := httptest.NewRequest("POST", "/hooks/plain", bytes.NewReader([]byte("build finished")))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	var env struct {
		Payload struct {
			Text string `json:"text"`
		} `json:"payload"`
	}
	_ = json.Unmarshal((*delivered)[0], &env)
	if env.Payload.Text != "build finished" {
		t.Fatalf("want the body wrapped as std/text@1, got %+v", env.Payload)
	}
}
