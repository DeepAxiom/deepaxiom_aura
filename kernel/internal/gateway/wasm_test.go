package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/executor"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
	"aura/kernel/internal/wasmrt"
)

// Reuses wasmrt's own test guest (../wasmrt/testdata/guest) rather than a
// second fixture — this package's job is proving the *wiring* around a
// real sandboxed module works end to end, not re-proving the sandbox
// itself, which internal/wasmrt's own tests already do exhaustively.
var (
	guestOnce  sync.Once
	guestBytes []byte
	guestErr   error
)

func buildGuest(t *testing.T) []byte {
	t.Helper()
	guestOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "aura-gateway-test-guest")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			guestErr = err
			return
		}
		out := filepath.Join(dir, "guest.wasm")
		cmd := exec.Command("go", "build", "-o", out, "../wasmrt/testdata/guest")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if raw, err := cmd.CombinedOutput(); err != nil {
			guestErr = err
			t.Logf("go build (GOOS=wasip1 GOARCH=wasm) failed:\n%s", raw)
			return
		}
		guestBytes, guestErr = os.ReadFile(out)
	})
	if guestErr != nil {
		t.Skipf("cannot build the wasip1 test guest in this environment: %v", guestErr)
	}
	return guestBytes
}

// testGatewayWithWasm is testGateway (gateway_test.go) plus a real
// wasmrt.Runtime wired in — the one thing every test in this file needs
// that the shared fixture doesn't provide.
func testGatewayWithWasm(t *testing.T) (*Gateway, http.Handler) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rt, err := wasmrt.New(context.Background())
	if err != nil {
		t.Fatalf("wasmrt.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	reg := registry.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{
		Node: &identity.Node{ID: "node-test", Mode: identity.ModeLocal},
		Reg:  reg, St: st,
		Mgr:  executor.NewManager(reg, st, string(identity.ModeLocal), nil, nil, log),
		Adm:  NewAdmission(0),
		Wasm: rt,
		Log:  log,
	}
	return g, g.Handler()
}

func wasmSkillManifest(id string) map[string]any {
	return map[string]any{
		"id": id, "version": "1.0.0", "protocol": "1",
		"name": id, "description": "test wasm skill",
		"capability": "logical.uppercase", "type": "logical", "format": "wasm",
		"ports": map[string]any{
			"ingress": []map[string]any{{"name": "text_in", "schema": "std/text@1"}},
			"egress":  []map[string]any{{"name": "text_out", "schema": "std/text@1"}},
		},
	}
}

// --- registration --------------------------------------------------------

func TestRegisterWasmSkillReturns404WhenNoRuntimeWired(t *testing.T) {
	_, h := testGateway(t) // the plain fixture: Wasm is nil
	code, _ := do(t, h, "POST", "/v1/skills/wasm", map[string]any{
		"manifest": wasmSkillManifest("acme/logical/upper"), "wasm_b64": "",
	})
	if code != 404 {
		t.Fatalf("status = %d, want 404 for a node with no wasm runtime", code)
	}
}

func TestRegisterWasmSkillRejectsTheWrongPortCount(t *testing.T) {
	_, h := testGatewayWithWasm(t)
	m := wasmSkillManifest("acme/logical/upper")
	m["ports"] = map[string]any{
		"ingress": []map[string]any{
			{"name": "a_in", "schema": "std/text@1"},
			{"name": "b_in", "schema": "std/text@1"},
		},
		"egress": []map[string]any{{"name": "text_out", "schema": "std/text@1"}},
	}
	code, body := do(t, h, "POST", "/v1/skills/wasm", map[string]any{
		"manifest": m, "wasm_b64": base64.StdEncoding.EncodeToString(buildGuest(t)),
	})
	if code != 422 {
		t.Fatalf("status = %d, want 422, body=%v", code, body)
	}
}

func TestRegisterWasmSkillCompilesAndRegisters(t *testing.T) {
	g, h := testGatewayWithWasm(t)
	code, body := do(t, h, "POST", "/v1/skills/wasm", map[string]any{
		"manifest": wasmSkillManifest("acme/logical/upper"),
		"wasm_b64": base64.StdEncoding.EncodeToString(buildGuest(t)),
	})
	if code != 201 {
		t.Fatalf("status = %d, want 201, body=%v", code, body)
	}
	if _, err := g.Reg.Resolve("acme/logical/upper", ""); err != nil {
		t.Fatalf("registered skill is not resolvable live: %v", err)
	}
}

// --- end to end: a real session delivers to it and gets a real reply -------

func TestWasmSkillDeliveryRoundTripsThroughASession(t *testing.T) {
	g, h := testGatewayWithWasm(t)
	code, _ := do(t, h, "POST", "/v1/skills/wasm", map[string]any{
		"manifest": wasmSkillManifest("acme/logical/upper"),
		"wasm_b64": base64.StdEncoding.EncodeToString(buildGuest(t)),
	})
	if code != 201 {
		t.Fatalf("registration failed: %d", code)
	}

	graph := map[string]any{
		"ir": executor.IRMajor, "graph_id": "wasm-t", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "u", "resolve": "logical.uppercase"}},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "u.text_in"},
			{"from": "u.text_out", "to": "client.text_in"},
		},
	}
	raw, _ := json.Marshal(graph)
	if err := g.St.SaveGraph("wasm-t", raw); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	var mu sync.Mutex
	var toClient []channel.Envelope
	sess, err := g.Mgr.Start("sess-wasm", "wasm-t", "", func(rawEnv []byte, _ string) error {
		var env channel.Envelope
		_ = json.Unmarshal(rawEnv, &env)
		mu.Lock()
		toClient = append(toClient, env)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}

	payload, _ := json.Marshal(map[string]any{"mode": "echo", "text": "hi there"})
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: "sess-wasm",
		Node: "client", Port: "text_out", Seq: 1, Idem: "sess-wasm:1",
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(toClient)
		mu.Unlock()
		if n >= 2 { // data, then done
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(toClient) < 2 {
		t.Fatalf("client received %d envelope(s), want at least 2 (data, done)", len(toClient))
	}
	var got struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(toClient[0].Payload, &got); err != nil {
		t.Fatalf("unmarshal reply payload %q: %v", toClient[0].Payload, err)
	}
	if !got.OK || got.Text != "HI THERE" {
		t.Fatalf("reply = %+v, want ok=true text=\"HI THERE\"", got)
	}
	if toClient[0].Kind != channel.KindData {
		t.Fatalf("first envelope kind = %q, want data", toClient[0].Kind)
	}
	if toClient[1].Kind != channel.KindDone {
		t.Fatalf("second envelope kind = %q, want done", toClient[1].Kind)
	}
}

// A guest that exits non-zero must surface as a channel.KindError, not a
// silently dropped delivery.
func TestWasmSkillErrorSurfacesToTheClient(t *testing.T) {
	g, h := testGatewayWithWasm(t)
	code, _ := do(t, h, "POST", "/v1/skills/wasm", map[string]any{
		"manifest": wasmSkillManifest("acme/logical/upper"),
		"wasm_b64": base64.StdEncoding.EncodeToString(buildGuest(t)),
	})
	if code != 201 {
		t.Fatalf("registration failed: %d", code)
	}

	graph := map[string]any{
		"ir": executor.IRMajor, "graph_id": "wasm-err", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "u", "resolve": "logical.uppercase"}},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "u.text_in"},
			{"from": "u.text_out", "to": "client.text_in"},
		},
	}
	raw, _ := json.Marshal(graph)
	if err := g.St.SaveGraph("wasm-err", raw); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	var mu sync.Mutex
	var toClient []channel.Envelope
	sess, err := g.Mgr.Start("sess-wasm-err", "wasm-err", "", func(rawEnv []byte, _ string) error {
		var env channel.Envelope
		_ = json.Unmarshal(rawEnv, &env)
		mu.Lock()
		toClient = append(toClient, env)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}

	payload, _ := json.Marshal(map[string]any{"mode": "fail"})
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: "sess-wasm-err",
		Node: "client", Port: "text_out", Seq: 1, Idem: "sess-wasm-err:1",
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(toClient)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(toClient) != 1 || toClient[0].Kind != channel.KindError {
		t.Fatalf("client received %+v, want exactly one error envelope", toClient)
	}
}
