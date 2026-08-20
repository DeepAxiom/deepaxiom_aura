package registry

import (
	"testing"
	"time"
)

func validManifest() Manifest {
	m := Manifest{
		ID: "acme/logical/uppercase", Version: "1.0.0", Protocol: "1",
		Name: "Uppercase", Description: "Uppercases the text it receives.",
		Capability: "logical.uppercase", Type: "logical", Format: "source",
	}
	m.Ports.Ingress = []Port{{Name: "text_in", Schema: "std/text@1"}}
	m.Ports.Egress = []Port{{Name: "text_out", Schema: "std/text@1"}}
	return m
}

func TestManifestValidateAcceptsAWellFormedManifest(t *testing.T) {
	m := validManifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("want valid, got %v", err)
	}
}

func TestManifestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"id without org/category/name", func(m *Manifest) { m.ID = "uppercase" }},
		{"id with uppercase letters", func(m *Manifest) { m.ID = "Acme/Logical/Uppercase" }},
		{"non-semver version", func(m *Manifest) { m.Version = "1.0" }},
		{"missing protocol", func(m *Manifest) { m.Protocol = "" }},
		{"missing name", func(m *Manifest) { m.Name = "" }},
		// The planner reads the description to decide whether to use a skill,
		// so an empty one is a functional defect, not a style nit.
		{"missing description", func(m *Manifest) { m.Description = "" }},
		{"capability outside the taxonomy", func(m *Manifest) { m.Capability = "wizardry.summon" }},
		{"unknown type", func(m *Manifest) { m.Type = "oracle" }},
		{"capability not prefixed by type", func(m *Manifest) { m.Type = "motor" }},
		{"missing format", func(m *Manifest) { m.Format = "" }},
		{"no ports at all", func(m *Manifest) { m.Ports.Ingress = nil; m.Ports.Egress = nil }},
		{"port without a schema", func(m *Manifest) {
			m.Ports.Ingress = []Port{{Name: "text_in"}}
		}},
		{"schema without a major", func(m *Manifest) {
			m.Ports.Ingress = []Port{{Name: "text_in", Schema: "std/text"}}
		}},
		{"invalid port name", func(m *Manifest) {
			m.Ports.Ingress = []Port{{Name: "text-in", Schema: "std/text@1"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

// --- compensates (C4 groundwork) ---------------------------------------------

func motorManifest() Manifest {
	m := Manifest{
		ID: "acme/motor/writer", Version: "1.0.0", Protocol: "1",
		Name: "Writer", Description: "Writes to the ERP.",
		Capability: "motor.erp.write", Type: "motor", Format: "source",
	}
	m.Ports.Ingress = []Port{
		{Name: "text_in", Schema: "std/text@1"},
		{Name: "undo_in", Schema: "acme/erp-undo@1"},
	}
	m.Ports.Egress = []Port{{Name: "text_out", Schema: "std/text@1"}}
	return m
}

func TestCompensatesAcceptsAWellFormedDeclaration(t *testing.T) {
	m := motorManifest()
	m.Compensates = &Compensation{Port: "undo_in", Schema: "acme/erp-undo@1"}
	if err := m.Validate(); err != nil {
		t.Fatalf("want valid, got %v", err)
	}
}

// The field is metadata for the ledger, not a live capability, but a port it
// names has to actually exist — otherwise a future `aura undo` calls a port
// that was never declared.
func TestCompensatesPortMustBeDeclared(t *testing.T) {
	m := motorManifest()
	m.Compensates = &Compensation{Port: "no_such_port"}
	if err := m.Validate(); err == nil {
		t.Fatal("want an error for a compensates.port that is not a declared ingress port")
	}
}

func TestCompensatesSchemaMustMatchTheDeclaredPort(t *testing.T) {
	m := motorManifest()
	m.Compensates = &Compensation{Port: "undo_in", Schema: "std/text@1"} // undo_in declares acme/erp-undo@1
	if err := m.Validate(); err == nil {
		t.Fatal("want an error when compensates.schema disagrees with the port's own declared schema")
	}
}

// An omitted schema just means "trust the port's own declaration" — it is not
// an error, since the schema is redundant information the port already carries.
func TestCompensatesSchemaIsOptional(t *testing.T) {
	m := motorManifest()
	m.Compensates = &Compensation{Port: "undo_in"}
	if err := m.Validate(); err != nil {
		t.Fatalf("an omitted compensates.schema should be accepted, got %v", err)
	}
}

// Only an effect can be compensated. A logical or cognitive skill declaring
// this would be meaningless — there is no effect on the world to undo.
func TestCompensatesIsRejectedOnNonMotorSkills(t *testing.T) {
	m := validManifest() // type: logical
	m.Ports.Ingress = append(m.Ports.Ingress, Port{Name: "undo_in", Schema: "std/text@1"})
	m.Compensates = &Compensation{Port: "undo_in", Schema: "std/text@1"}
	if err := m.Validate(); err == nil {
		t.Fatal("want an error when a non-motor skill declares compensates")
	}
}

func f64(v float64) *float64 { return &v }

func configManifest() Manifest {
	m := validManifest()
	m.Config = []ConfigParam{
		{Key: "temperature", Type: "float", Default: 0.7, Min: f64(0), Max: f64(2)},
		{Key: "max_tokens", Type: "int", Default: 512, Min: f64(1)},
		{Key: "system_prompt", Type: "string", Default: "hi"},
		{Key: "verbose", Type: "bool", Default: false},
		{Key: "mode", Type: "enum", Default: "disabled", Options: []string{"disabled", "dry-run", "live"}},
	}
	return m
}

func TestValidateConfigValue(t *testing.T) {
	m := configManifest()
	cases := []struct {
		name    string
		key     string
		value   any
		wantErr bool
	}{
		{"float in range", "temperature", 1.0, false},
		{"float below min", "temperature", -0.1, true},
		{"float above max", "temperature", 2.5, true},
		{"int accepted", "max_tokens", float64(2048), false},
		{"int rejects a fraction", "max_tokens", 1.5, true},
		{"int below min", "max_tokens", float64(0), true},
		{"string accepted", "system_prompt", "you are helpful", false},
		{"string rejects a number", "system_prompt", 3.0, true},
		{"bool accepted", "verbose", true, false},
		{"bool rejects a string", "verbose", "true", true},
		{"enum member accepted", "mode", "live", false},
		{"enum non-member rejected", "mode", "yolo", true},
		// Config is capability-based like permissions: undeclared is denied.
		{"undeclared key rejected", "sneaky", "value", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := m.ValidateConfigValue(tc.key, tc.value)
			if tc.wantErr && err == nil {
				t.Fatalf("want an error for %s=%v, got nil", tc.key, tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want %s=%v accepted, got %v", tc.key, tc.value, err)
			}
		})
	}
}

// Precedence is the whole point of the layering: manifest default is the
// floor, the --config file overrides it, a live override beats both.
func TestEffectiveConfigPrecedence(t *testing.T) {
	m := configManifest()

	got := m.EffectiveConfig(nil, nil)
	if got["temperature"] != 0.7 {
		t.Fatalf("want the declared default with no layers, got %v", got["temperature"])
	}

	got = m.EffectiveConfig(map[string]any{"temperature": 0.3}, nil)
	if got["temperature"] != 0.3 {
		t.Fatalf("want the file layer to beat the default, got %v", got["temperature"])
	}

	got = m.EffectiveConfig(map[string]any{"temperature": 0.3}, map[string]any{"temperature": 1.1})
	if got["temperature"] != 1.1 {
		t.Fatalf("want the store layer to beat the file, got %v", got["temperature"])
	}

	// Keys the manifest never declared must not leak in from either layer.
	got = m.EffectiveConfig(map[string]any{"ghost": 1}, map[string]any{"phantom": 2})
	if _, ok := got["ghost"]; ok {
		t.Fatal("undeclared key from the file layer leaked into the effective config")
	}
	if _, ok := got["phantom"]; ok {
		t.Fatal("undeclared key from the store layer leaked into the effective config")
	}
}

func live(id, capability string, connected time.Time) *Live {
	m := validManifest()
	m.ID = id
	m.Capability = capability
	return &Live{Manifest: m, Connected: connected, Send: func([]byte, string) error { return nil }}
}

func TestResolveByCapabilityExactAndPrefix(t *testing.T) {
	r := New()
	r.Register("c1", live("acme/sensorial/ocr", "sensorial.ocr.image", time.Now()))

	if _, err := r.Resolve("", "sensorial.ocr.image"); err != nil {
		t.Fatalf("exact capability should resolve: %v", err)
	}
	// A graph may demand a broader capability than the skill advertises.
	if _, err := r.Resolve("", "sensorial.ocr"); err != nil {
		t.Fatalf("prefix capability should resolve: %v", err)
	}
	if _, err := r.Resolve("", "sensorial.asr"); err == nil {
		t.Fatal("an unrelated capability must not resolve")
	}
}

func TestResolveByPackageID(t *testing.T) {
	r := New()
	r.Register("c1", live("acme/sensorial/ocr", "sensorial.ocr.image", time.Now()))

	if _, err := r.Resolve("acme/sensorial/ocr", ""); err != nil {
		t.Fatalf("exact package id should resolve: %v", err)
	}
	if _, err := r.Resolve("acme/sensorial/other", ""); err == nil {
		t.Fatal("an unknown package id must not resolve")
	}
}

// Resolution has to be deterministic, or the same graph would behave
// differently run to run when two instances of a skill are connected.
func TestResolvePrefersTheNewestConnection(t *testing.T) {
	r := New()
	old := time.Now().Add(-time.Hour)
	r.Register("old", live("acme/logical/a", "logical.dup", old))
	r.Register("new", live("acme/logical/b", "logical.dup", time.Now()))

	for i := 0; i < 20; i++ {
		got, err := r.Resolve("", "logical.dup")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.Manifest.ID != "acme/logical/b" {
			t.Fatalf("want the newest connection to win every time, got %q", got.Manifest.ID)
		}
	}
}

func TestResolveFailsWhenNothingIsConnected(t *testing.T) {
	r := New()
	if _, err := r.Resolve("", "logical.echo"); err == nil {
		t.Fatal("want an explained error when no skill provides the capability")
	}
}

func TestUnregisterRemovesTheSkill(t *testing.T) {
	r := New()
	r.Register("c1", live("acme/logical/a", "logical.echo", time.Now()))
	r.Unregister("c1")

	if _, err := r.Resolve("", "logical.echo"); err == nil {
		t.Fatal("want resolution to fail after the connection is unregistered")
	}
	if n := len(r.Catalog()); n != 0 {
		t.Fatalf("want an empty catalog, got %d entries", n)
	}
}

// SendToID is how a config_update reaches a running skill; two instances of
// the same id must both be told.
func TestSendToIDReachesEveryConnectionOfThatSkill(t *testing.T) {
	r := New()
	delivered := 0
	mk := func() *Live {
		l := live("acme/logical/a", "logical.echo", time.Now())
		l.Send = func([]byte, string) error { delivered++; return nil }
		return l
	}
	r.Register("c1", mk())
	r.Register("c2", mk())
	r.Register("other", live("acme/logical/b", "logical.other", time.Now()))

	if n := r.SendToID("acme/logical/a", []byte("{}")); n != 2 {
		t.Fatalf("want 2 connections told, got %d", n)
	}
	if delivered != 2 {
		t.Fatalf("want 2 deliveries, got %d", delivered)
	}
	if n := r.SendToID("acme/nobody/here", []byte("{}")); n != 0 {
		t.Fatalf("want 0 for an unknown skill id, got %d", n)
	}
}

func TestCatalogIsSortedByID(t *testing.T) {
	r := New()
	r.Register("c1", live("zeta/logical/z", "logical.z", time.Now()))
	r.Register("c2", live("alpha/logical/a", "logical.a", time.Now()))

	cat := r.Catalog()
	if len(cat) != 2 || cat[0].ID != "alpha/logical/a" {
		t.Fatalf("want the catalog sorted by id, got %+v", cat)
	}
}

// Replicas of the same package must share the load, or running more instances
// of a skill buys nothing: one capability would be one connection, and one
// connection is one serialized writer.
func TestResolveRoundRobinsAcrossReplicas(t *testing.T) {
	r := New()
	now := time.Now()
	for _, c := range []string{"r1", "r2", "r3"} {
		r.Register(c, live("acme/logical/rep", "logical.rep", now))
	}
	seen := map[*Live]int{}
	for i := 0; i < 30; i++ {
		got, err := r.Resolve("", "logical.rep")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		seen[got]++
	}
	if len(seen) != 3 {
		t.Fatalf("load reached %d of 3 replicas; the others were idle", len(seen))
	}
	for l, n := range seen {
		if n != 10 {
			t.Errorf("replica %p served %d of 30 sessions; want an even 10", l, n)
		}
	}
}

// Rotation must not leak across packages: a different implementation of the
// same capability is an ambiguity, not a replica, and it stays deterministic.
func TestReplicaRotationDoesNotBreakDeterminismAcrossPackages(t *testing.T) {
	r := New()
	old := time.Now().Add(-time.Hour)
	now := time.Now()
	r.Register("old", live("acme/logical/a", "logical.mix", old))
	r.Register("new1", live("acme/logical/b", "logical.mix", now))
	r.Register("new2", live("acme/logical/b", "logical.mix", now))

	for i := 0; i < 20; i++ {
		got, err := r.Resolve("", "logical.mix")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.Manifest.ID != "acme/logical/b" {
			t.Fatalf("the older package answered: %q", got.Manifest.ID)
		}
	}
}

// Replicas is what a report about the catalogue reads; Register answers the
// same question for one connection as it happens. They must agree.
func TestReplicasCountsConnectionsPerSkill(t *testing.T) {
	r := New()
	live := func(id string) *Live {
		var m Manifest
		m.ID = id
		m.Capability = "logical.echo"
		m.Type = "logical"
		return &Live{Manifest: m}
	}
	if n := r.Register("c1", live("a/b/one")); n != 1 {
		t.Fatalf("first connection reported %d instances", n)
	}
	if n := r.Register("c2", live("a/b/one")); n != 2 {
		t.Fatalf("second connection to the same skill reported %d", n)
	}
	if n := r.Register("c3", live("a/b/two")); n != 1 {
		t.Fatalf("a different skill reported %d", n)
	}

	got := r.Replicas()
	if got["a/b/one"] != 2 || got["a/b/two"] != 1 {
		t.Fatalf("Replicas = %v", got)
	}

	r.Unregister("c2")
	if r.Replicas()["a/b/one"] != 1 {
		t.Fatal("a disconnected replica must stop being counted")
	}
}
