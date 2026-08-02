package mcpsrv

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// mcpsrv is one of the four milestones that rested entirely on hand
// verification. It is also the surface through which an external agent —
// Claude Code, Cursor — reaches this node's skills, so "an effect over MCP
// always needs a human" is a claim that has to be checked rather than
// asserted.

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type nodeStub struct {
	srv *httptest.Server

	mu        sync.Mutex
	authSeen  []string
	graphs    []map[string]any
	skills    []map[string]any
	graphCode int
}

func newNodeStub(t *testing.T, skills []map[string]any) *nodeStub {
	t.Helper()
	n := &nodeStub{skills: skills, graphCode: 201}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		n.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(n.skills)
	})
	mux.HandleFunc("/v1/graphs", func(w http.ResponseWriter, r *http.Request) {
		n.record(r)
		var g map[string]any
		_ = json.NewDecoder(r.Body).Decode(&g)
		n.mu.Lock()
		n.graphs = append(n.graphs, g)
		code := n.graphCode
		n.mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":"refused"}`))
	})
	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

func (n *nodeStub) record(r *http.Request) {
	n.mu.Lock()
	n.authSeen = append(n.authSeen, r.Header.Get("Authorization"))
	n.mu.Unlock()
}

func (n *nodeStub) lastGraph() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.graphs) == 0 {
		return nil
	}
	return n.graphs[len(n.graphs)-1]
}

func skill(id, capability, skillType string) map[string]any {
	return map[string]any{
		"id": id, "name": id, "description": "a test skill",
		"capability": capability, "type": skillType,
		"ports": map[string]any{
			"ingress": []map[string]string{{"name": "text_in", "schema": "std/text@1"}},
			"egress":  []map[string]string{{"name": "text_out", "schema": "std/text@1"}},
		},
	}
}

func call(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	s.Handler()(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON-RPC: %s", rec.Body.String())
	}
	return out
}

// --- JSON-RPC plumbing --------------------------------------------------------

func TestInitializeAnnouncesTheProtocol(t *testing.T) {
	node := newNodeStub(t, nil)
	s := &Server{BaseURL: node.srv.URL, Version: "9.9.9", Log: testLogger()}

	res, _ := call(t, s, "initialize", nil)["result"].(map[string]any)
	if res["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %q", res["protocolVersion"], protocolVersion)
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["version"] != "9.9.9" {
		t.Errorf("serverInfo.version = %v; it should report the node's version", info["version"])
	}
}

func TestUnknownMethodIsAJSONRPCError(t *testing.T) {
	node := newNodeStub(t, nil)
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	out := call(t, s, "no/such/method", nil)
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("want a JSON-RPC error, got %v", out)
	}
	if e["code"].(float64) != -32601 {
		t.Errorf("code = %v, want -32601 (method not found)", e["code"])
	}
}

func TestMalformedJSONIsAParseError(t *testing.T) {
	node := newNodeStub(t, nil)
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	s.Handler()(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	e, ok := out["error"].(map[string]any)
	if !ok || e["code"].(float64) != -32700 {
		t.Fatalf("want a -32700 parse error, got %v", out)
	}
}

// A notification carries no id and must get no body — a client that receives
// one for a notification treats the stream as corrupt.
func TestNotificationGetsNoResponseBody(t *testing.T) {
	node := newNodeStub(t, nil)
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	rec := httptest.NewRecorder()
	s.Handler()(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 for a notification", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("a notification got a body: %s", rec.Body.String())
	}
}

func TestGetIsRejected(t *testing.T) {
	node := newNodeStub(t, nil)
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	s.Handler()(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 (no SSE stream)", rec.Code)
	}
}

// --- skills become tools -------------------------------------------------------

func TestSkillsBecomeTools(t *testing.T) {
	node := newNodeStub(t, []map[string]any{
		skill("acme/logical/echo", "logical.echo", "logical"),
		skill("acme/motor/writer", "motor.api.writer", "motor"),
	})
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	res, _ := call(t, s, "tools/list", nil)["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("got %d tools for 2 skills", len(tools))
	}

	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		names[tool["name"].(string)] = true
		if tool["inputSchema"] == nil {
			t.Errorf("tool %v has no inputSchema; a client cannot call it", tool["name"])
		}
	}
	// MCP tool names must match ^[a-zA-Z0-9_-]{1,64}$, so dots are replaced.
	if !names["logical_echo"] {
		t.Errorf("capability was not sanitised into a valid MCP tool name: %v", names)
	}
}

func TestToolNameSanitisesCapabilities(t *testing.T) {
	for capability, want := range map[string]string{
		"logical.echo":             "logical_echo",
		"motor.api.writer":         "motor_api_writer",
		"sensorial.asr.transcribe": "sensorial_asr_transcribe",
		"cognitive.llm/chat":       "cognitive_llm_chat",
	} {
		if got := toolName(capability); got != want {
			t.Errorf("toolName(%q) = %q, want %q", capability, got, want)
		}
		if len(want) > 64 {
			t.Errorf("%q exceeds the MCP name limit", want)
		}
	}
}

func TestInputSchemaFollowsThePortSchema(t *testing.T) {
	text := inputSchema("std/text@1")
	props, _ := text["properties"].(map[string]any)
	if _, ok := props["text"]; !ok {
		t.Errorf("std/text@1 did not produce a text property: %v", text)
	}

	doc := inputSchema("std/document@1")
	props, _ = doc["properties"].(map[string]any)
	if _, ok := props["bytes_b64"]; !ok {
		t.Errorf("std/document@1 did not produce a bytes_b64 property: %v", doc)
	}

	// An unknown schema still yields a usable object rather than nothing.
	other := inputSchema("acme/custom@3")
	if other["type"] != "object" {
		t.Errorf("an unknown schema produced %v", other)
	}
}

func TestUnreachableNodeIsReportedNotSilent(t *testing.T) {
	s := &Server{BaseURL: "http://127.0.0.1:1", Log: testLogger()}
	out := call(t, s, "tools/list", nil)
	if _, ok := out["error"]; !ok {
		t.Fatalf("a dead node produced a successful tools/list: %v", out)
	}
}

// --- the safety claim ----------------------------------------------------------

// The README's claim is that an effect reached over MCP always carries a human
// gate. An external agent cannot answer a gate, so the practical consequence is
// that acting through MCP is refused rather than silently performed — and this
// is the test that says so.
func TestMotorToolGraphCarriesAHumanGate(t *testing.T) {
	node := newNodeStub(t, []map[string]any{
		skill("acme/motor/writer", "motor.api.writer", "motor"),
	})
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	// The call cannot complete (the stub serves no websocket), but the graph is
	// registered before the stream is opened, which is what we are inspecting.
	_ = call(t, s, "tools/call", map[string]any{
		"name":      "motor_api_writer",
		"arguments": map[string]any{"text": "delete everything"},
	})

	graph := node.lastGraph()
	if graph == nil {
		t.Fatal("no graph was registered for the tool call")
	}
	edges, _ := graph["edges"].([]any)
	var gated bool
	for _, raw := range edges {
		e, _ := raw.(map[string]any)
		to, _ := e["to"].(string)
		if strings.HasPrefix(to, "s.") && e["gate"] == "human-approval" {
			gated = true
		}
	}
	if !gated {
		t.Fatalf("the edge into a motor skill carries no human-approval gate: %v", edges)
	}
}

func TestNonMotorToolGraphIsNotGated(t *testing.T) {
	node := newNodeStub(t, []map[string]any{
		skill("acme/logical/echo", "logical.echo", "logical"),
	})
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	_ = call(t, s, "tools/call", map[string]any{
		"name":      "logical_echo",
		"arguments": map[string]any{"text": "hello"},
	})

	graph := node.lastGraph()
	if graph == nil {
		t.Fatal("no graph was registered")
	}
	for _, raw := range graph["edges"].([]any) {
		e, _ := raw.(map[string]any)
		if e["gate"] != nil {
			t.Fatalf("a non-effect edge was gated: %v", e)
		}
	}
}

func TestCallingAnUnknownToolIsAnError(t *testing.T) {
	node := newNodeStub(t, []map[string]any{
		skill("acme/logical/echo", "logical.echo", "logical"),
	})
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	out := call(t, s, "tools/call", map[string]any{"name": "no_such_tool"})
	if _, ok := out["error"]; !ok {
		t.Fatalf("calling a tool that does not exist succeeded: %v", out)
	}
}

// A refused graph used to be discarded, so the failure surfaced later as an
// unexplained timeout instead of as the refusal it was.
func TestRefusedGraphIsReportedImmediately(t *testing.T) {
	node := newNodeStub(t, []map[string]any{
		skill("acme/logical/echo", "logical.echo", "logical"),
	})
	node.graphCode = 422
	s := &Server{BaseURL: node.srv.URL, Log: testLogger()}

	out := call(t, s, "tools/call", map[string]any{
		"name": "logical_echo", "arguments": map[string]any{"text": "hi"},
	})
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("a refused graph produced no error: %v", out)
	}
	if !strings.Contains(e["message"].(string), "refused") {
		t.Errorf("the error does not carry the node's reason: %v", e["message"])
	}
}

// --- authentication ------------------------------------------------------------

// The MCP server is a client of the public API with no kernel privileges, so
// once the node requires a token it needs one like anybody else.
func TestServerPresentsItsTokenToTheNode(t *testing.T) {
	node := newNodeStub(t, []map[string]any{
		skill("acme/logical/echo", "logical.echo", "logical"),
	})
	s := &Server{BaseURL: node.srv.URL, Token: "node-tok", Log: testLogger()}

	_ = call(t, s, "tools/list", nil)

	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.authSeen) == 0 {
		t.Fatal("the MCP server never called the node")
	}
	for _, h := range node.authSeen {
		if h != "Bearer node-tok" {
			t.Fatalf("call carried %q; want the node token", h)
		}
	}
}

func TestRejectedTokenIsExplained(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/skills", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &Server{BaseURL: srv.URL, Token: "wrong", Log: testLogger()}
	out := call(t, s, "tools/list", nil)
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("a 401 from the node produced a successful listing: %v", out)
	}
	if !strings.Contains(strings.ToLower(e["message"].(string)), "token") {
		t.Errorf("the error should mention the token: %v", e["message"])
	}
}
