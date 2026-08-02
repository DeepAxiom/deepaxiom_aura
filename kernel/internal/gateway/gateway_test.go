package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aura/kernel/internal/executor"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
)

func testGateway(t *testing.T) (*Gateway, http.Handler) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	reg := registry.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{
		Node: &identity.Node{ID: "node-test", Mode: identity.ModeLocal},
		Reg:  reg,
		St:   st,
		Mgr:  executor.NewManager(reg, st, string(identity.ModeLocal), nil, nil, log),
		Adm:  NewAdmission(0),
		Log:  log,
	}
	return g, g.Handler()
}

func do(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// registerLive puts a skill in the live registry and records whatever the
// kernel pushes to it, which is how a config_update is observed.
func registerLive(g *Gateway, id string, cfg []registry.ConfigParam) *[][]byte {
	pushed := &[][]byte{}
	m := registry.Manifest{
		ID: id, Version: "1.0.0", Protocol: "1",
		Name: id, Description: "test skill",
		Capability: "logical.test", Type: "logical", Format: "source",
		Config: cfg,
	}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	g.Reg.Register("conn-"+id, &registry.Live{
		Manifest: m, Connected: time.Now(),
		Send: func(raw []byte, _ string) error { *pushed = append(*pushed, raw); return nil },
	})
	return pushed
}

func TestHealthzReportsIdentityAndProtocol(t *testing.T) {
	_, h := testGateway(t)
	code, body := do(t, h, "GET", "/healthz", nil)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["ok"] != true || body["node"] != "node-test" || body["mode"] != "local" {
		t.Fatalf("healthz body = %v", body)
	}
	if body["protocol"] == nil || body["ir"] == nil {
		t.Fatal("healthz must declare the protocol and IR majors it speaks")
	}
}

func TestRegisterGraphAcceptsValidIRAndRejectsInvalid(t *testing.T) {
	_, h := testGateway(t)

	valid := map[string]any{
		"ir": "1", "graph_id": "echo",
		"origin": map[string]string{"kind": "declared"},
		"nodes":  []map[string]any{{"ref": "eco", "resolve": "logical.echo"}},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "eco.text_in"},
			{"from": "eco.text_out", "to": "client.text_in"},
		},
	}
	if code, _ := do(t, h, "POST", "/v1/graphs", valid); code != 201 {
		t.Fatalf("valid IR: status = %d, want 201", code)
	}

	code, _ := do(t, h, "GET", "/v1/graphs", nil)
	if code != 200 {
		t.Fatalf("list graphs: status = %d", code)
	}

	// An IR major the executor does not speak is a client error, not a 500.
	bad := map[string]any{"ir": "99", "graph_id": "x",
		"origin": map[string]string{"kind": "declared"},
		"nodes":  []map[string]any{{"ref": "a", "resolve": "logical.x"}},
		"edges":  []map[string]any{{"from": "client.text_out", "to": "a.text_in"}}}
	if code, _ := do(t, h, "POST", "/v1/graphs", bad); code != 422 {
		t.Fatalf("unsupported IR major: status = %d, want 422", code)
	}

	if code, _ := do(t, h, "GET", "/v1/graphs/nope", nil); code != 404 {
		t.Fatalf("unknown graph: status = %d, want 404", code)
	}
}

func TestGetSkillConfigReturnsSchemaAndEffectiveValues(t *testing.T) {
	g, h := testGateway(t)
	registerLive(g, "acme/logical/test", []registry.ConfigParam{
		{Key: "mode", Type: "enum", Default: "disabled",
			Options: []string{"disabled", "dry-run", "live"}},
	})

	if code, _ := do(t, h, "GET", "/v1/skills/config", nil); code != 400 {
		t.Fatal("want 400 when ?id= is missing")
	}
	if code, _ := do(t, h, "GET", "/v1/skills/config?id=acme/nobody/here", nil); code != 404 {
		t.Fatal("want 404 for a skill this node has never seen")
	}

	code, body := do(t, h, "GET", "/v1/skills/config?id=acme/logical/test", nil)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["schema"] == nil {
		t.Fatal("want the declared config schema so the UI can render a form")
	}
	values, _ := body["values"].(map[string]any)
	if values["mode"] != "disabled" {
		t.Fatalf("want the declared default as the effective value, got %v", values["mode"])
	}
}

// This is the path `aura promote` rides on: the kernel validates the value,
// persists it, and pushes it to the running skill.
func TestPutSkillConfigValidatesPersistsAndPushesLive(t *testing.T) {
	g, h := testGateway(t)
	pushed := registerLive(g, "acme/logical/test", []registry.ConfigParam{
		{Key: "mode", Type: "enum", Default: "disabled",
			Options: []string{"disabled", "dry-run", "live"}},
	})

	// The kernel rejects a value outside the declared enum — the skill is
	// never asked to police its own config.
	code, body := do(t, h, "PUT", "/v1/skills/config?id=acme/logical/test",
		map[string]any{"mode": "yolo"})
	if code != 422 {
		t.Fatalf("invalid enum value: status = %d, want 422", code)
	}
	if body["error"] == nil {
		t.Fatal("want an explanation of why the value was refused")
	}

	// Undeclared keys are denied like undeclared permissions.
	if code, _ := do(t, h, "PUT", "/v1/skills/config?id=acme/logical/test",
		map[string]any{"undeclared": 1}); code != 422 {
		t.Fatalf("undeclared key: status = %d, want 422", code)
	}

	code, body = do(t, h, "PUT", "/v1/skills/config?id=acme/logical/test",
		map[string]any{"mode": "live"})
	if code != 200 {
		t.Fatalf("valid value: status = %d, want 200", code)
	}
	if body["pushed_live"] != true {
		t.Fatal("a connected skill must be told about the change over its own connection")
	}
	if len(*pushed) != 1 {
		t.Fatalf("want 1 config_update pushed, got %d", len(*pushed))
	}
	var env map[string]any
	_ = json.Unmarshal((*pushed)[0], &env)
	if env["kind"] != "config_update" {
		t.Fatalf("pushed envelope kind = %v, want config_update", env["kind"])
	}

	// And it survives: the override is stored, not just held in memory.
	_, body = do(t, h, "GET", "/v1/skills/config?id=acme/logical/test", nil)
	values, _ := body["values"].(map[string]any)
	if values["mode"] != "live" {
		t.Fatalf("want the override persisted, got %v", values["mode"])
	}
}

// A skill that is offline can still be configured; the value waits for it.
func TestSkillConfigWorksWhileTheSkillIsDisconnected(t *testing.T) {
	g, h := testGateway(t)
	registerLive(g, "acme/logical/test", []registry.ConfigParam{
		{Key: "mode", Type: "enum", Default: "disabled",
			Options: []string{"disabled", "live"}},
	})
	// Persist the manifest the way skillWS does, then drop the connection.
	live, _ := g.Reg.Resolve("acme/logical/test", "")
	if err := g.St.UpsertSkill(live.Manifest.ID, live.Manifest.Version, live.Manifest); err != nil {
		t.Fatalf("UpsertSkill: %v", err)
	}
	g.Reg.Unregister("conn-acme/logical/test")

	code, body := do(t, h, "PUT", "/v1/skills/config?id=acme/logical/test",
		map[string]any{"mode": "live"})
	if code != 200 {
		t.Fatalf("status = %d, want 200 for an offline skill", code)
	}
	if body["pushed_live"] != false {
		t.Fatal("nothing is connected, so pushed_live must be false")
	}
}

// A projection's headers carry its credentials. Listing them would hand the
// keys to the projected system to anyone who can read the control-plane UI.
func TestListProjectionsRedactsCredentials(t *testing.T) {
	g, h := testGateway(t)

	stored := `{"name":"crm","base_url":"https://crm.internal",
	            "headers":{"Authorization":"Bearer super-secret-token"},
	            "ops":[{"op_id":"list-users","method":"GET","path":"/users","mode":"live"}]}`
	if err := g.St.SaveProjection("crm", []byte(stored)); err != nil {
		t.Fatalf("SaveProjection: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/projections", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(body, "super-secret-token") {
		t.Fatalf("the projection's credential was served to the client:\n%s", body)
	}
	// The header's existence is still visible, so an operator can tell that
	// auth is configured at all.
	if !strings.Contains(body, "Authorization") {
		t.Fatalf("want the header name kept so auth is visibly configured:\n%s", body)
	}
	if !strings.Contains(body, "list-users") {
		t.Fatalf("want the operations still listed:\n%s", body)
	}
}

func TestAgentCardDescribesTheLiveCatalog(t *testing.T) {
	g, h := testGateway(t)
	registerLive(g, "acme/logical/test", nil)

	code, body := do(t, h, "GET", "/.well-known/agent.json", nil)
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	skills, _ := body["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("want the live catalog in the A2A card, got %v", body["skills"])
	}
}
