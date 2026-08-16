package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"aura/kernel/internal/mcpcli"
	"aura/kernel/internal/spec"
)

// ── config ──────────────────────────────────────────────────────────

// The whole adoption argument rests on this: the file an operator already has
// must work unmodified.
func TestParsesTheConfigDesktopClientsAlreadyWrite(t *testing.T) {
	raw := []byte(`{
	  "mcpServers": {
	    "filesystem": {
	      "command": "npx",
	      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
	      "env": {"FOO": "bar"}
	    },
	    "remote": {"url": "https://example.com/mcp", "headers": {"X-Key": "abc"}}
	  }
	}`)
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(cfg.Servers))
	}
	fs := cfg.Servers["filesystem"]
	if fs.transport() != "stdio" || fs.Command != "npx" || fs.Env["FOO"] != "bar" {
		t.Errorf("stdio server parsed wrong: %+v", fs)
	}
	if got := cfg.Servers["remote"]; got.transport() != "http" || got.Headers["X-Key"] != "abc" {
		t.Errorf("http server parsed wrong: %+v", got)
	}
	if fs.Name != "filesystem" {
		t.Errorf("Name should be back-filled from the key, got %q", fs.Name)
	}
}

func TestConfigRefusesAServerWithNoTransport(t *testing.T) {
	_, err := ParseConfig([]byte(`{"mcpServers": {"broken": {"args": ["x"]}}}`))
	if err == nil || !strings.Contains(err.Error(), "neither") {
		t.Fatalf("want an explained refusal, got %v", err)
	}
}

func TestConfigRefusesAnEmptyDocument(t *testing.T) {
	if _, err := ParseConfig([]byte(`{}`)); err == nil {
		t.Fatal("an empty config must not silently guard nothing")
	}
}

// ── the safety-critical classification ──────────────────────────────

func ptr(b bool) *bool { return &b }

// Everything hinges on this table: a tool that is typed sensorial is NOT gated,
// so any row that yields sensorial when it should not is a hole straight
// through the guard.
func TestToolTypingDefaultsToGated(t *testing.T) {
	cases := []struct {
		name  string
		tool  mcpcli.Tool
		trust bool
		want  string
	}{
		{"no annotations", mcpcli.Tool{Name: "t"}, false, spec.TypeMotor},
		{"no annotations, trusting", mcpcli.Tool{Name: "t"}, true, spec.TypeMotor},
		{"readOnly but not trusted", mcpcli.Tool{Name: "t",
			Annotations: &mcpcli.Annotations{ReadOnlyHint: ptr(true)}}, false, spec.TypeMotor},
		{"readOnly and trusted", mcpcli.Tool{Name: "t",
			Annotations: &mcpcli.Annotations{ReadOnlyHint: ptr(true)}}, true, spec.TypeSensorial},
		{"destructive and trusted", mcpcli.Tool{Name: "t",
			Annotations: &mcpcli.Annotations{ReadOnlyHint: ptr(true), DestructiveHint: ptr(true)}},
			true, spec.TypeMotor},
		{"explicitly not readOnly, trusted", mcpcli.Tool{Name: "t",
			Annotations: &mcpcli.Annotations{ReadOnlyHint: ptr(false)}}, true, spec.TypeMotor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := manifestFor("srv", tc.tool, tc.trust)
			if m.Type != tc.want {
				t.Errorf("type = %q, want %q", m.Type, tc.want)
			}
			if !strings.HasPrefix(m.Capability, tc.want+".") {
				t.Errorf("capability %q must carry the %q prefix", m.Capability, tc.want)
			}
		})
	}
}

// A manifest the registry would reject means the tool silently never registers,
// which fails open — the agent keeps its unguarded path. So every shape a real
// server might hand us has to survive validation.
func TestGeneratedManifestsAreValid(t *testing.T) {
	names := []string{
		"read_file", "GetIssue", "list-dirs", "search.web",
		"weird__name!!", "ALLCAPS", "a", "тест",
		strings.Repeat("very-long-tool-name", 12),
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			m := manifestFor("My Server/2", mcpcli.Tool{Name: n, Description: "d"}, false)
			if err := m.Validate(); err != nil {
				t.Fatalf("manifest for %q is invalid: %v\n%+v", n, err, m)
			}
		})
	}
}

func TestManifestUsesTheProjectionSchemas(t *testing.T) {
	m := manifestFor("srv", mcpcli.Tool{Name: "t"}, false)
	if m.Format != "projection" {
		t.Errorf("format = %q; nothing about this tool runs on this node", m.Format)
	}
	if m.Ports.Ingress[0].Schema != "std/api-request@1" ||
		m.Ports.Egress[0].Schema != "std/api-response@1" {
		t.Errorf("ports should reuse the connected-system schemas, got %+v", m.Ports)
	}
}

// The description is what a planner and a model read, so the posture has to be
// legible there and not only in the type field.
func TestDescriptionStatesThePosture(t *testing.T) {
	gated := manifestFor("s", mcpcli.Tool{Name: "t", Description: "does a thing"}, false)
	if !strings.Contains(gated.Description, "gated") {
		t.Errorf("a gated tool should say so: %q", gated.Description)
	}
	ro := manifestFor("s", mcpcli.Tool{Name: "t", Description: "reads",
		Annotations: &mcpcli.Annotations{ReadOnlyHint: ptr(true)}}, true)
	if !strings.Contains(ro.Description, "read-only") {
		t.Errorf("a read-only tool should say so: %q", ro.Description)
	}
}

func TestBindingGatedTracksType(t *testing.T) {
	if !(Binding{Type: spec.TypeMotor}).Gated() {
		t.Error("a motor binding must report as gated")
	}
	if (Binding{Type: spec.TypeSensorial}).Gated() {
		t.Error("a sensorial binding must not report as gated")
	}
}

// Two different tools must never collide onto one capability, or one would
// shadow the other and calls would silently reach the wrong server.
func TestDistinctToolsGetDistinctCapabilities(t *testing.T) {
	seen := map[string]string{}
	for _, srv := range []string{"github", "git-hub"} {
		for _, tool := range []string{"read_file", "read-file", "write_file"} {
			m := manifestFor(srv, mcpcli.Tool{Name: tool}, false)
			key := m.Capability
			if prev, dup := seen[key]; dup {
				t.Errorf("%s/%s collides with %s on %q", srv, tool, prev, key)
			}
			seen[key] = srv + "/" + tool
		}
	}
}

// ── argument extraction ─────────────────────────────────────────────

func TestArgumentExtraction(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    map[string]any
		wantErr bool
	}{
		{"body wins", `{"body":{"path":"/tmp"},"query":{"x":1}}`,
			map[string]any{"path": "/tmp"}, false},
		{"params when there is no body", `{"params":{"id":"7"}}`,
			map[string]any{"id": "7"}, false},
		{"a bare object passes through", `{"path":"/tmp"}`,
			map[string]any{"path": "/tmp"}, false},
		{"envelope fields are not arguments", `{"path":"/tmp","query":{"a":1},"headers":{"b":2}}`,
			map[string]any{"path": "/tmp"}, false},
		{"empty payload", ``, map[string]any{}, false},
		{"a non-object body is refused", `{"body":"not an object"}`, nil, true},
		{"invalid json is refused", `{[}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := arguments([]byte(tc.payload))
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("args = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestSlugsStayWithinTheContractsAlphabets(t *testing.T) {
	if got := slugID("Weird Name/2!!"); strings.ContainsAny(got, "_ /!") {
		t.Errorf("slugID leaked a character an id may not carry: %q", got)
	}
	if got := slugCap("Weird Name/2!!"); strings.ContainsAny(got, " /!") {
		t.Errorf("slugCap leaked a character a capability may not carry: %q", got)
	}
	if slugID("") == "" || slugCap("") == "" {
		t.Error("an empty name must still produce a usable segment")
	}
	if slugID("!!!") == "" || slugCap("---") == "" {
		t.Error("an all-punctuation name must still produce a usable segment")
	}
}
