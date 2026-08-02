package projection

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The OpenAPI projection turns someone else's API into skills. Its four
// safeties — read-only by default, dry-run before live, writes typed as motor,
// and the declared egress host — are the reason `aura connect` is safe to point
// at a production system, and none of them had a test.

const petstore = `{
  "openapi": "3.0.0",
  "info": {"title": "Petstore", "version": "1.0.0"},
  "servers": [{"url": "https://api.example.com/v1"}],
  "paths": {
    "/pets": {
      "get": {
        "operationId": "listPets",
        "summary": "List all pets",
        "parameters": [
          {"name": "limit", "in": "query", "schema": {"type": "integer"}}
        ]
      },
      "post": {
        "operationId": "createPet",
        "summary": "Create a pet"
      }
    },
    "/pets/{petId}": {
      "get": {
        "operationId": "showPetById",
        "summary": "Info for a specific pet",
        "parameters": [
          {"name": "petId", "in": "path", "required": true, "schema": {"type": "string"}}
        ]
      },
      "delete": {
        "operationId": "deletePet",
        "summary": "Remove a pet"
      }
    }
  }
}`

func parse(t *testing.T, name, baseURL string, spec string) *Config {
	t.Helper()
	cfg, err := ParseOpenAPI(name, baseURL, nil, []byte(spec))
	if err != nil {
		t.Fatalf("ParseOpenAPI: %v", err)
	}
	return cfg
}

func opByID(cfg *Config, id string) *Op {
	for i := range cfg.Ops {
		if cfg.Ops[i].OpID == id {
			return &cfg.Ops[i]
		}
	}
	return nil
}

// --- parsing --------------------------------------------------------------------

func TestEveryOperationBecomesAnOp(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)

	if len(cfg.Ops) != 4 {
		t.Fatalf("got %d ops for 4 operations: %+v", len(cfg.Ops), cfg.Ops)
	}
	for _, id := range []string{"list-pets", "create-pet", "show-pet-by-id", "delete-pet"} {
		if opByID(cfg, id) == nil {
			t.Errorf("operation %q was dropped", id)
		}
	}
}

func TestBaseURLComesFromTheSpecUnlessOverridden(t *testing.T) {
	fromSpec := parse(t, "petstore", "", petstore)
	if fromSpec.BaseURL != "https://api.example.com/v1" {
		t.Errorf("BaseURL = %q; want the spec's server", fromSpec.BaseURL)
	}

	overridden := parse(t, "petstore", "http://localhost:8080", petstore)
	if overridden.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q; an explicit --base-url must win", overridden.BaseURL)
	}
}

func TestMethodAndPathAreCarried(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)

	op := opByID(cfg, "show-pet-by-id")
	if op.Method != "GET" || op.Path != "/pets/{petId}" {
		t.Errorf("show-pet-by-id = %s %s; want GET /pets/{petId}", op.Method, op.Path)
	}
	if opByID(cfg, "create-pet").Method != "POST" {
		t.Errorf("create-pet method = %q", opByID(cfg, "create-pet").Method)
	}
}

func TestParametersAreCarriedWithTheirLocation(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)

	op := opByID(cfg, "show-pet-by-id")
	if len(op.Params) != 1 {
		t.Fatalf("show-pet-by-id has %d params; want 1", len(op.Params))
	}
	// The planner reads `in` to know whether to substitute into the path or
	// append to the query, so losing it makes the operation uncallable.
	if op.Params[0].Name != "petId" || op.Params[0].In != "path" {
		t.Errorf("param = %+v; want petId in path", op.Params[0])
	}
}

func TestInvalidSpecIsRejected(t *testing.T) {
	for name, spec := range map[string]string{
		"not json": "{nope",
		"no paths": `{"openapi":"3.0.0","info":{"title":"x","version":"1"}}`,
		"empty":    ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOpenAPI("x", "", nil, []byte(spec)); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// --- the four safeties ------------------------------------------------------------

// Safety 1 and 2: a write is typed `motor` and starts *disabled* — not merely
// dry-run — so pointing `aura connect` at a production API cannot reach a write
// path at all until someone promotes that operation on purpose.
func TestWritesAreMotorAndStartDisabled(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)

	for _, id := range []string{"create-pet", "delete-pet"} {
		op := opByID(cfg, id)
		if !op.Write {
			t.Errorf("%s is not marked as a write; it would be typed sensorial and ungated", id)
		}
		if op.Mode != ModeDisabled {
			t.Errorf("%s starts in mode %q; a write must start disabled", id, op.Mode)
		}
	}
}

// Safety 3: reads are live immediately, because a projection whose reads do
// nothing is not worth connecting.
func TestReadsAreLiveAndNotWrites(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)

	for _, id := range []string{"list-pets", "show-pet-by-id"} {
		op := opByID(cfg, id)
		if op.Write {
			t.Errorf("%s was classified as a write", id)
		}
		if op.Mode != ModeLive {
			t.Errorf("%s starts in mode %q; reads should be usable at once", id, op.Mode)
		}
	}
}

// Safety 4: the generated manifest types a write as `motor`, which is what
// makes the kernel's policy engine gate it.
func TestGeneratedManifestTypesWritesAsMotor(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)
	h := &Host{cfg: *cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	write := h.manifest(*opByID(cfg, "create-pet"))
	if write["type"] != "motor" {
		t.Errorf("a write projected as type %v; the gate keys on this", write["type"])
	}
	if !strings.HasPrefix(write["capability"].(string), "motor.") {
		t.Errorf("write capability = %v; it must carry the motor prefix", write["capability"])
	}

	read := h.manifest(*opByID(cfg, "list-pets"))
	if read["type"] != "sensorial" {
		t.Errorf("a read projected as type %v; want sensorial", read["type"])
	}
}

// The declared egress host is what a future sandbox will enforce, and what the
// registry shows at install time today. A projection that declared no host
// would be asking for unrestricted network access.
func TestManifestDeclaresOnlyTheTargetHost(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)
	h := &Host{cfg: *cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := h.manifest(*opByID(cfg, "list-pets"))
	perms, _ := m["permissions"].(map[string]any)
	hosts, _ := perms["egress_http"].([]string)
	if len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Errorf("egress_http = %v; want only the API's own host", perms["egress_http"])
	}
	if perms["filesystem"] != "none" {
		t.Errorf("filesystem = %v; a projection needs no disk", perms["filesystem"])
	}
}

// A dry-run operation says so in its description, because that text is what a
// planner reads to decide whether to use it.
func TestDryRunIsVisibleInTheDescription(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)
	h := &Host{cfg: *cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Writes ship disabled, so promote this one to dry-run first — that is the
	// state whose description a planner has to be able to read.
	op := *opByID(cfg, "create-pet")
	op.Mode = ModeDryRun
	m := h.manifest(op)
	if !strings.Contains(strings.ToUpper(m["description"].(string)), "DRY-RUN") {
		t.Errorf("a dry-run op does not say so where a planner reads: %v", m["description"])
	}
}

func TestDeclaredParamsAreListedForThePlanner(t *testing.T) {
	cfg := parse(t, "petstore", "", petstore)
	h := &Host{cfg: *cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := h.manifest(*opByID(cfg, "show-pet-by-id"))
	desc := m["description"].(string)
	if !strings.Contains(desc, "path:petId") {
		t.Errorf("the planner cannot see the declared parameters: %v", desc)
	}
}

// --- execution --------------------------------------------------------------------

// A dry-run must describe the call and never make it. This is the safety the
// whole promote workflow rests on.
func TestDryRunDescribesWithoutCalling(t *testing.T) {
	var called bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer api.Close()

	h := &Host{
		cfg:    Config{Name: "x", BaseURL: api.URL},
		client: api.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	op := Op{OpID: "createPet", Method: "POST", Path: "/pets", Write: true, Mode: ModeDryRun}

	out := h.execute(op, map[string]any{"body": map[string]any{"name": "rex"}})

	if called {
		t.Fatal("a dry-run reached the upstream API")
	}
	if out["dry_run"] != true {
		t.Errorf("result does not declare itself a dry run: %v", out)
	}
	req, _ := out["request"].(map[string]any)
	if req == nil || !strings.Contains(req["url"].(string), "/pets") {
		t.Errorf("a dry run should describe the call it would make: %v", out)
	}
}

func TestLiveOperationCallsUpstream(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pets/42" {
			t.Errorf("path param was not substituted: %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "10" {
			t.Errorf("query was not appended: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"rex"}`))
	}))
	defer api.Close()

	h := &Host{
		cfg:    Config{Name: "x", BaseURL: api.URL},
		client: api.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	op := Op{OpID: "showPetById", Method: "GET", Path: "/pets/{petId}", Mode: ModeLive}

	out := h.execute(op, map[string]any{
		"params": map[string]any{"petId": "42"},
		"query":  map[string]any{"limit": 10},
	})

	if out["ok"] != true {
		t.Fatalf("call failed: %v", out)
	}
	body, _ := out["body"].(map[string]any)
	if body["name"] != "rex" {
		t.Errorf("body = %v", out["body"])
	}
}

func TestUpstreamErrorIsReportedNotPanicked(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer api.Close()

	h := &Host{
		cfg:    Config{Name: "x", BaseURL: api.URL},
		client: api.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	out := h.execute(Op{Method: "GET", Path: "/x", Mode: ModeLive}, nil)

	if out["ok"] != false {
		t.Errorf("a 500 was reported as ok: %v", out)
	}
	if out["status"] != 500 {
		t.Errorf("status = %v; the caller needs the real code", out["status"])
	}
}

func TestUnreachableUpstreamIsReported(t *testing.T) {
	h := &Host{
		cfg:    Config{Name: "x", BaseURL: "http://127.0.0.1:1"},
		client: http.DefaultClient,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	out := h.execute(Op{Method: "GET", Path: "/x", Mode: ModeLive}, nil)
	if out["ok"] != false || out["error"] == nil {
		t.Errorf("an unreachable API produced %v", out)
	}
}

// Configured headers carry credentials; a per-request header may refine them.
// Both have to reach the wire or an authenticated API is unusable.
func TestConfiguredAndRequestHeadersAreSent(t *testing.T) {
	var gotAuth, gotTrace string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTrace = r.Header.Get("X-Trace")
		w.WriteHeader(200)
	}))
	defer api.Close()

	h := &Host{
		cfg: Config{Name: "x", BaseURL: api.URL,
			Headers: map[string]string{"Authorization": "Bearer upstream-key"}},
		client: api.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_ = h.execute(Op{Method: "GET", Path: "/x", Mode: ModeLive},
		map[string]any{"headers": map[string]any{"X-Trace": "abc"}})

	if gotAuth != "Bearer upstream-key" {
		t.Errorf("configured header did not reach upstream: %q", gotAuth)
	}
	if gotTrace != "abc" {
		t.Errorf("request header did not reach upstream: %q", gotTrace)
	}
}

// A response that is not JSON must come back as text rather than breaking the
// envelope — plenty of real APIs answer with HTML on an error path.
func TestNonJSONResponseIsCarriedAsText(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>down for maintenance</html>"))
	}))
	defer api.Close()

	h := &Host{
		cfg:    Config{Name: "x", BaseURL: api.URL},
		client: api.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	out := h.execute(Op{Method: "GET", Path: "/x", Mode: ModeLive}, nil)

	body, ok := out["body"].(string)
	if !ok || !strings.Contains(body, "maintenance") {
		t.Errorf("a non-JSON body was lost: %v", out["body"])
	}
}

// --- naming -------------------------------------------------------------------------

func TestSlugMakesCapabilitySafeNames(t *testing.T) {
	for in, want := range map[string]string{
		"petstore":  "petstore",
		"Pet Store": "pet-store",
		"my-api.v2": "my-api-v2",
		"UPPER":     "upper",
		// camelCase splits on the lower→upper transition, so an operationId
		// stays readable instead of collapsing to one word.
		"listPets":    "list-pets",
		"showPetById": "show-pet-by-id",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// The capability has to survive C1 validation, or every projected operation is
// rejected at registration.
func TestProjectedCapabilityIsWellFormed(t *testing.T) {
	cfg := parse(t, "Pet Store", "", petstore)
	h := &Host{cfg: *cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for _, op := range cfg.Ops {
		m := h.manifest(op)
		capability, _ := m["capability"].(string)
		for _, ch := range capability {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789._-", ch) {
				t.Fatalf("capability %q contains %q, which C1 rejects", capability, ch)
			}
		}
		var probe map[string]any
		raw, _ := json.Marshal(m)
		if json.Unmarshal(raw, &probe) != nil {
			t.Fatalf("manifest for %s is not serialisable", op.OpID)
		}
	}
}
