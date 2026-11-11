package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aura/kernel/internal/registry"
)

func get(t *testing.T, h http.Handler, path string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.Bytes()
}

func statsReplica(reg *registry.Registry, conn, id, capability string, at time.Time) *registry.Live {
	m := registry.Manifest{ID: id, Version: "1.0.0", Protocol: "1", Name: id,
		Description: "test", Capability: capability, Type: "logical", Format: "source"}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	m.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}
	l := &registry.Live{Manifest: m, Connected: at, Send: func([]byte, string) error { return nil }}
	reg.Register(conn, l)
	return l
}

func TestStatsReportsReplicaLoad(t *testing.T) {
	g, h := testGateway(t)
	base := time.Unix(1700000000, 0)
	a := statsReplica(g.Reg, "c1", "acme/logical/echo", "logical.echo", base)
	b := statsReplica(g.Reg, "c2", "acme/logical/echo", "logical.echo", base.Add(time.Second))
	statsReplica(g.Reg, "c3", "acme/logical/other", "logical.other", base)
	a.Acquire()
	a.Acquire()
	b.Acquire()

	code, body := get(t, h, "/v1/stats")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/stats: %d %s", code, body)
	}
	var got struct {
		At        int64 `json:"at"`
		Sessions  int   `json:"sessions_live"`
		Expertise []struct {
			ID       string  `json:"id"`
			Replicas []int64 `json:"replicas"`
			Sessions int64   `json:"sessions"`
		} `json:"expertise"`
		Mailbox *struct {
			Enabled bool `json:"enabled"`
		} `json:"mailbox"`
		Tokens *struct {
			Input uint64 `json:"input_tokens"`
		} `json:"tokens"`
		Traces *json.RawMessage `json:"traces"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("stats is not JSON: %v\n%s", err, body)
	}
	if got.At == 0 || got.Mailbox == nil || got.Tokens == nil {
		t.Fatalf("stats is missing its fixed members: %s", body)
	}
	if got.Traces != nil {
		t.Fatal("a node with no exporter reports trace stats; absent must mean not in use")
	}
	if len(got.Expertise) != 2 {
		t.Fatalf("two packages are connected, stats lists %d", len(got.Expertise))
	}
	echo := got.Expertise[0]
	if echo.ID != "acme/logical/echo" || len(echo.Replicas) != 2 ||
		echo.Replicas[0] != 2 || echo.Replicas[1] != 1 || echo.Sessions != 3 {
		t.Fatalf("replica load reads %+v; want [2 1] in connection order, 3 sessions", echo)
	}
	if other := got.Expertise[1]; other.ID != "acme/logical/other" || other.Sessions != 0 {
		t.Fatalf("idle package reads %+v", other)
	}
}

// The control surface under its current name answers exactly what the
// original paths answer, and the originals still answer.
func TestExpertiseRoutesAliasTheSkillRoutes(t *testing.T) {
	g, h := testGateway(t)
	statsReplica(g.Reg, "c1", "acme/logical/echo", "logical.echo", time.Now())

	_, oldBody := get(t, h, "/v1/skills")
	code, newBody := get(t, h, "/v1/expertise")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/expertise: %d", code)
	}
	if string(oldBody) != string(newBody) || !strings.Contains(string(newBody), "acme/logical/echo") {
		t.Fatalf("the two catalog routes disagree:\n old %s\n new %s", oldBody, newBody)
	}

	oldCode, oldCfg := get(t, h, "/v1/skills/config?id=acme/logical/echo")
	newCode, newCfg := get(t, h, "/v1/expertise/config?id=acme/logical/echo")
	if oldCode != newCode || string(oldCfg) != string(newCfg) {
		t.Fatalf("config routes disagree: %d %s vs %d %s", oldCode, oldCfg, newCode, newCfg)
	}
}

// A capability-scoped credential reaches its socket under either name and
// still reaches nothing an operator owns.
func TestScopedCredentialUnderTheNewNames(t *testing.T) {
	p := Principal{Scope: ScopeSkill, Capability: "motor.erp.write"}
	for _, path := range []string{"/ws/skill", "/ws/expertise"} {
		if !p.Allows(http.MethodGet, path) {
			t.Errorf("a scoped credential cannot open %s", path)
		}
	}
	for _, path := range []string{"/v1/expertise", "/v1/stats", "/v1/skills"} {
		if p.Allows(http.MethodGet, path) {
			t.Errorf("a scoped credential can read %s; only the operator should", path)
		}
	}
	if !isWebSocketPath("/ws/expertise") {
		t.Error("/ws/expertise is not treated as a WebSocket path, so its Origin is never checked")
	}
}

func TestStatsAndAliasesRequireTheToken(t *testing.T) {
	h := (&Auth{Token: "s3cret"}).Authenticate(okHandler())
	for _, path := range []string{"/v1/stats", "/v1/expertise", "/v1/expertise/config?id=x", "/ws/expertise"} {
		if code := request(t, h, http.MethodGet, path, nil); code != http.StatusUnauthorized {
			t.Errorf("%s without a token returned %d; want 401", path, code)
		}
		if code := request(t, h, http.MethodGet, path, bearer("s3cret")); code != http.StatusOK {
			t.Errorf("%s with the token returned %d; want 200", path, code)
		}
	}
}

func TestMetricsCarryTheNewSeries(t *testing.T) {
	g, h := testGateway(t)
	g.Mgr.SetMailboxDepth(64)
	code, body := get(t, h, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: %d", code)
	}
	for _, series := range []string{
		`aura_tokens_total{kind="input"} 0`,
		`aura_tokens_total{kind="output"} 0`,
		`aura_tokens_total{kind="cached_input"} 0`,
		"aura_inferences_metered_total 0",
		"aura_mailbox_queued 0",
		"aura_mailbox_waits_total 0",
	} {
		if !strings.Contains(string(body), series) {
			t.Errorf("/metrics is missing %q", series)
		}
	}
	if strings.Contains(string(body), "aura_trace_spans") {
		t.Error("a node with no exporter emits trace series")
	}
}
