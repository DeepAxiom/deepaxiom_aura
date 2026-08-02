package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/executor"
	"aura/kernel/internal/gateway"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// The end-to-end claim this package exists to make: an agent's tool call
// reaches the real MCP server only after the kernel has authorized it, and a
// tool that acts on the world does not move without a human.
//
// Everything below drives the real gateway, the real executor and the real
// policy — the only fake is the upstream MCP server, which stands in for
// whatever the operator actually runs.

// upstream is a fake MCP server that records what it was asked to do.
type upstream struct {
	*httptest.Server
	mu     sync.Mutex
	called []string
}

func (u *upstream) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.called...)
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18",
				"serverInfo": map[string]any{"name": "fake-fs", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{
					"name": "delete_everything", "description": "removes files",
				},
				map[string]any{
					"name": "read_file", "description": "reads a file",
					"annotations": map[string]any{"readOnlyHint": true},
				},
			}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			u.mu.Lock()
			u.called = append(u.called, fmt.Sprintf("%s(%v)", p.Name, p.Arguments["path"]))
			u.mu.Unlock()
			result = map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "did " + p.Name}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(u.Close)
	return u
}

// node stands up a real kernel over HTTP.
func node(t *testing.T) (*gateway.Gateway, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	reg := registry.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &gateway.Gateway{
		Node: &identity.Node{ID: "node-guard-test", Mode: identity.ModeLocal},
		Reg:  reg, St: st,
		Mgr: executor.NewManager(reg, st, string(identity.ModeLocal),
			executor.DefaultPolicy(), nil, log),
		Adm: gateway.NewAdmission(0),
		Log: log,
	}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return g, srv
}

// startGuard runs a guard against the node and waits for its tools to register.
func startGuard(t *testing.T, g *gateway.Gateway, node *httptest.Server,
	up *upstream, trust bool) *Guard {
	t.Helper()

	cfg, err := ParseConfig([]byte(fmt.Sprintf(
		`{"mcpServers":{"fs":{"url":%q}}}`, up.URL)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	gd := New(cfg, Options{
		BaseURL: node.URL, TrustAnnotations: trust,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = gd.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(g.Reg.Catalog()) >= 2 {
			return gd
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("guard did not register its tools; registry has %d", len(g.Reg.Catalog()))
	return nil
}

func TestGuardRegistersUpstreamToolsAsSkills(t *testing.T) {
	up := newUpstream(t)
	g, node := node(t)
	gd := startGuard(t, g, node, up, true)

	byCap := map[string]registry.Manifest{}
	for _, m := range g.Reg.Catalog() {
		byCap[m.Capability] = m
	}
	write, ok := byCap["motor.mcp.fs.delete_everything"]
	if !ok {
		t.Fatalf("the unannotated tool did not register as motor; got %v", keys(byCap))
	}
	if write.Type != spec.TypeMotor {
		t.Errorf("delete_everything type = %q, want motor", write.Type)
	}
	read, ok := byCap["sensorial.mcp.fs.read_file"]
	if !ok {
		t.Fatalf("the readOnly tool did not register as sensorial; got %v", keys(byCap))
	}
	if read.Type != spec.TypeSensorial {
		t.Errorf("read_file type = %q, want sensorial", read.Type)
	}

	bs := gd.Bindings()
	if len(bs) != 2 {
		t.Fatalf("bindings = %d, want 2", len(bs))
	}
	for _, b := range bs {
		if b.Tool == "delete_everything" && !b.Gated() {
			t.Error("a tool that was never declared read-only must be gated")
		}
	}
}

// Without --trust-annotations, a server claiming readOnlyHint gets gated
// anyway. This is the property that stops a hostile server disarming the guard
// by asserting it is harmless.
func TestUntrustedAnnotationsStillGate(t *testing.T) {
	up := newUpstream(t)
	g, node := node(t)
	startGuard(t, g, node, up, false)

	for _, m := range g.Reg.Catalog() {
		if m.Type != spec.TypeMotor {
			t.Errorf("%s registered as %q; without --trust-annotations everything is motor",
				m.Capability, m.Type)
		}
	}
}

// The whole point, proven end to end: the upstream server is not touched until
// a human approves, and it *is* touched once one does.
func TestGatedCallReachesUpstreamOnlyAfterApproval(t *testing.T) {
	up := newUpstream(t)
	g, node := node(t)
	startGuard(t, g, node, up, true)

	graph := map[string]any{
		"ir": "1", "graph_id": "guarded", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "t", "resolve": "motor.mcp.fs.delete_everything"}},
		"edges": []map[string]any{
			{"from": "client.call", "to": "t.call"},
			{"from": "t.result", "to": "client.result"},
		},
	}
	body, _ := json.Marshal(graph)
	resp, err := http.Post(node.URL+"/v1/graphs", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("register graph: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("register graph: %d %s", resp.StatusCode, b)
	}

	wsURL := strings.Replace(node.URL, "http", "ws", 1) + "/v1/stream?graph=guarded"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))

	var hello channel.Envelope
	if err := conn.ReadJSON(&hello); err != nil {
		t.Fatalf("hello: %v", err)
	}

	payload, _ := json.Marshal(map[string]any{"body": map[string]any{"path": "/etc"}})
	if err := conn.WriteJSON(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Node: "client", Port: "call", Seq: 1, Idem: "guard-test-1",
		Schema: "std/api-request@1", Kind: channel.KindData, Payload: payload,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// The gate must arrive before anything reaches the upstream server.
	var gate channel.Envelope
	for {
		if err := conn.ReadJSON(&gate); err != nil {
			t.Fatalf("waiting for the gate: %v", err)
		}
		if gate.Kind == channel.KindConfirmRequest {
			break
		}
		if gate.Kind == channel.KindData {
			t.Fatal("a result arrived with no approval — the gate did not hold")
		}
	}
	if got := up.calls(); len(got) != 0 {
		t.Fatalf("the upstream server was called before approval: %v", got)
	}

	approve, _ := json.Marshal(map[string]bool{"approve": true})
	if err := conn.WriteJSON(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: gate.ID,
		Kind: channel.KindConfirmResponse, Payload: approve,
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	var result channel.Envelope
	for {
		if err := conn.ReadJSON(&result); err != nil {
			t.Fatalf("waiting for the result: %v", err)
		}
		if result.Kind == channel.KindData {
			break
		}
	}
	var out struct {
		OK   bool   `json:"ok"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(result.Payload, &out); err != nil {
		t.Fatalf("result payload: %s", result.Payload)
	}
	if !out.OK || !strings.Contains(out.Body, "delete_everything") {
		t.Errorf("unexpected result: %s", result.Payload)
	}
	calls := up.calls()
	if len(calls) != 1 || calls[0] != "delete_everything(/etc)" {
		t.Fatalf("upstream calls = %v; want exactly the approved one", calls)
	}
}

// A denial must leave the world untouched.
func TestDeniedCallNeverReachesUpstream(t *testing.T) {
	up := newUpstream(t)
	g, node := node(t)
	startGuard(t, g, node, up, true)

	graph := map[string]any{
		"ir": "1", "graph_id": "denied", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "t", "resolve": "motor.mcp.fs.delete_everything"}},
		"edges": []map[string]any{
			{"from": "client.call", "to": "t.call"},
			{"from": "t.result", "to": "client.result"},
		},
	}
	body, _ := json.Marshal(graph)
	resp, _ := http.Post(node.URL+"/v1/graphs", "application/json", strings.NewReader(string(body)))
	if resp != nil {
		resp.Body.Close()
	}

	wsURL := strings.Replace(node.URL, "http", "ws", 1) + "/v1/stream?graph=denied"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))

	var hello channel.Envelope
	_ = conn.ReadJSON(&hello)
	payload, _ := json.Marshal(map[string]any{"body": map[string]any{"path": "/etc"}})
	_ = conn.WriteJSON(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Node: "client", Port: "call", Seq: 1, Idem: "guard-test-deny",
		Schema: "std/api-request@1", Kind: channel.KindData, Payload: payload,
	})

	var gate channel.Envelope
	for {
		if err := conn.ReadJSON(&gate); err != nil {
			t.Fatalf("waiting for the gate: %v", err)
		}
		if gate.Kind == channel.KindConfirmRequest {
			break
		}
	}
	deny, _ := json.Marshal(map[string]bool{"approve": false})
	_ = conn.WriteJSON(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: gate.ID,
		Kind: channel.KindConfirmResponse, Payload: deny,
	})

	// Give the executor room to do the wrong thing, if it were going to.
	time.Sleep(500 * time.Millisecond)
	if got := up.calls(); len(got) != 0 {
		t.Fatalf("a denied call still reached the upstream server: %v", got)
	}
}

// A read-only tool the operator has chosen to trust runs without a prompt —
// otherwise the strict default would make guarding unusable in practice.
func TestTrustedReadOnlyToolIsNotGated(t *testing.T) {
	up := newUpstream(t)
	g, node := node(t)
	startGuard(t, g, node, up, true)

	graph := map[string]any{
		"ir": "1", "graph_id": "reads", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "t", "resolve": "sensorial.mcp.fs.read_file"}},
		"edges": []map[string]any{
			{"from": "client.call", "to": "t.call"},
			{"from": "t.result", "to": "client.result"},
		},
	}
	body, _ := json.Marshal(graph)
	resp, _ := http.Post(node.URL+"/v1/graphs", "application/json", strings.NewReader(string(body)))
	if resp != nil {
		resp.Body.Close()
	}

	wsURL := strings.Replace(node.URL, "http", "ws", 1) + "/v1/stream?graph=reads"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))

	var hello channel.Envelope
	_ = conn.ReadJSON(&hello)
	payload, _ := json.Marshal(map[string]any{"body": map[string]any{"path": "/etc/hosts"}})
	_ = conn.WriteJSON(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Node: "client", Port: "call", Seq: 1, Idem: "guard-test-read",
		Schema: "std/api-request@1", Kind: channel.KindData, Payload: payload,
	})

	for {
		var env channel.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			t.Fatalf("waiting for the result: %v", err)
		}
		if env.Kind == channel.KindConfirmRequest {
			t.Fatal("a trusted read-only tool should not raise a gate")
		}
		if env.Kind == channel.KindData {
			break
		}
	}
	if got := up.calls(); len(got) != 1 || got[0] != "read_file(/etc/hosts)" {
		t.Fatalf("upstream calls = %v", got)
	}
}

func keys(m map[string]registry.Manifest) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
