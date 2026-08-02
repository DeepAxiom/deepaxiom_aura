package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
)

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aura.policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

func mustLoadPolicy(t *testing.T, body string) *Policy {
	t.Helper()
	p, err := LoadPolicy(writePolicy(t, body))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p
}

// --- matching ---------------------------------------------------------------

func TestMatchCapabilityForms(t *testing.T) {
	for _, tc := range []struct {
		match, capability string
		want              bool
	}{
		{"*", "motor.anything.at.all", true},
		{"motor.tts.speak", "motor.tts.speak", true},
		{"motor.tts.speak", "motor.tts.speaker", false}, // not a prefix match
		{"motor.erp.*", "motor.erp.invoice.create", true},
		{"motor.erp.*", "motor.erp", true},   // the bare prefix counts
		{"motor.erp.*", "motor.erpx", false}, // a sibling must not be swept in
		{"motor.*", "cognitive.llm.chat", false},
	} {
		if got := matchCapability(tc.match, tc.capability); got != tc.want {
			t.Errorf("matchCapability(%q, %q) = %v, want %v",
				tc.match, tc.capability, got, tc.want)
		}
	}
}

// Only motor.* falls to default_effect. Applying it to everything would gate
// every parser and every LLM call, which makes a node unusable rather than safe.
func TestDefaultEffectAppliesOnlyToEffects(t *testing.T) {
	p := mustLoadPolicy(t, "policy: 1\ndefault_effect: gate\n")

	if d, _, _ := p.Decide("motor.api.writer"); d != DecisionGate {
		t.Errorf("motor capability got %q, want gate", d)
	}
	if d, _, _ := p.Decide("cognitive.llm.chat"); d != DecisionAllow {
		t.Errorf("non-effect got %q, want allow", d)
	}
	if d, _, _ := p.Decide("motor"); d != DecisionGate {
		t.Errorf("the bare motor type got %q, want gate", d)
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	p := mustLoadPolicy(t, `
policy: 1
default_effect: deny
rules:
  - match: "motor.tts.speak"
    decision: allow
  - match: "motor.*"
    decision: gate
`)
	if d, _, _ := p.Decide("motor.tts.speak"); d != DecisionAllow {
		t.Errorf("specific rule should win, got %q", d)
	}
	if d, _, _ := p.Decide("motor.erp.write"); d != DecisionGate {
		t.Errorf("broad rule should catch the rest, got %q", d)
	}
}

// --- the hole this closes ---------------------------------------------------

// The regression that motivates the whole file. A graph is a JSON document
// anyone able to reach /v1/graphs can POST, so `"gate": "none"` used to let the
// author of a graph waive the kernel's strongest guarantee. Under a policy the
// node is the authority and the waiver is ignored.
func TestGraphWaiverIsIgnoredUnderAPolicy(t *testing.T) {
	p := mustLoadPolicy(t, "policy: 1\ndefault_effect: gate\n")

	gate, err := p.Authorize("motor.api.writer", GateNone)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if gate != GateHumanApproval {
		t.Fatalf("a graph waived the gate under a policy that did not grant it; got %q", gate)
	}
}

// ...and the same waiver is honoured when the node explicitly grants graphs
// that authority, because a node run by one developer on their laptop should
// not need a policy file to use the seeded voice graph.
func TestGraphWaiverIsHonouredWhenExplicitlyGranted(t *testing.T) {
	p := mustLoadPolicy(t, "policy: 1\ndefault_effect: gate\nallow_graph_waiver: true\n")

	gate, err := p.Authorize("motor.tts.speak", GateNone)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if gate != "" {
		t.Fatalf("an explicitly granted waiver was not honoured; got gate %q", gate)
	}
}

// The built-in default has to reproduce the pre-policy behaviour exactly, or
// `aura up` with no policy file stops being able to run its own seeded graphs.
func TestDefaultPolicyPreservesPrePolicyBehaviour(t *testing.T) {
	p := DefaultPolicy()

	if gate, err := p.Authorize("motor.tts.speak", GateNone); err != nil || gate != "" {
		t.Errorf("gate:none under the default policy: gate=%q err=%v; want delivered", gate, err)
	}
	if gate, err := p.Authorize("motor.api.writer", ""); err != nil || gate != GateHumanApproval {
		t.Errorf("an omitted gate on a motor edge: gate=%q err=%v; want human-approval", gate, err)
	}
	if gate, err := p.Authorize("logical.echo", ""); err != nil || gate != "" {
		t.Errorf("a non-effect: gate=%q err=%v; want delivered", gate, err)
	}
}

// --- the ordering rule ------------------------------------------------------

// A graph may be stricter than policy and never laxer. Both directions matter:
// the lax direction is the security hole, the strict direction is an author
// asking for more safety than they were required to have.
func TestGraphMayBeStricterButNeverLaxer(t *testing.T) {
	permissive := mustLoadPolicy(t, `
policy: 1
default_effect: gate
allow_graph_waiver: true
rules:
  - match: "motor.api.writer"
    decision: allow
`)
	// Stricter: policy allows, the author still wants a human to look.
	if gate, err := permissive.Authorize("motor.api.writer", GateHumanApproval); err != nil || gate != GateHumanApproval {
		t.Errorf("author asked for a gate policy did not require: gate=%q err=%v", gate, err)
	}
	// Laxer: policy denies, and no graph may talk its way out of that.
	strict := mustLoadPolicy(t, `
policy: 1
default_effect: gate
allow_graph_waiver: true
rules:
  - match: "motor.payments.*"
    decision: deny
    reason: "no automated payments on this node"
`)
	for _, graphGate := range []string{"", GateNone, GateHumanApproval} {
		if _, err := strict.Authorize("motor.payments.send", graphGate); err == nil {
			t.Errorf("a deny was overridden by graph gate %q", graphGate)
		}
	}
}

func TestDenyErrorNamesTheReason(t *testing.T) {
	p := mustLoadPolicy(t, `
policy: 1
rules:
  - match: "motor.payments.*"
    decision: deny
    reason: "no automated payments on this node"
`)
	_, err := p.Authorize("motor.payments.send", "")
	if err == nil {
		t.Fatal("want a denial")
	}
	// An operator reading a refused session needs to know which line did it.
	if got := err.Error(); !contains(got, "no automated payments on this node") {
		t.Errorf("denial does not carry its reason: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// --- rate limits ------------------------------------------------------------

func TestRateLimitRefusesPastTheWindow(t *testing.T) {
	p := mustLoadPolicy(t, `
policy: 1
default_effect: allow
rules:
  - match: "motor.erp.*"
    decision: allow
    limit: { per_minute: 3 }
`)
	for i := 1; i <= 3; i++ {
		if err := p.CheckRate("motor.erp.write"); err != nil {
			t.Fatalf("call %d refused inside the limit: %v", i, err)
		}
	}
	if err := p.CheckRate("motor.erp.write"); err == nil {
		t.Fatal("the fourth call in a 3/min window was allowed")
	}
	// A different capability has its own budget.
	if err := p.CheckRate("motor.other.write"); err != nil {
		t.Fatalf("an unlimited capability was refused: %v", err)
	}
}

func TestNoLimitMeansNoLimit(t *testing.T) {
	p := mustLoadPolicy(t, "policy: 1\ndefault_effect: allow\n")
	for i := 0; i < 100; i++ {
		if err := p.CheckRate("motor.api.writer"); err != nil {
			t.Fatalf("call %d refused with no limit declared: %v", i, err)
		}
	}
}

// --- loading ----------------------------------------------------------------

func TestLoadPolicyRejectsMalformedDocuments(t *testing.T) {
	for name, body := range map[string]string{
		"unsupported version": "policy: 2\n",
		"bad default":         "policy: 1\ndefault_effect: maybe\n",
		"empty match":         "policy: 1\nrules:\n  - match: \"\"\n    decision: allow\n",
		"bad decision":        "policy: 1\nrules:\n  - match: \"motor.*\"\n    decision: perhaps\n",
		"negative limit":      "policy: 1\nrules:\n  - match: \"motor.*\"\n    decision: allow\n    limit: { per_minute: -1 }\n",
		"not yaml":            "policy: 1\nrules: [oh dear\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPolicy(writePolicy(t, body)); err == nil {
				t.Fatalf("malformed policy (%s) was accepted", name)
			}
		})
	}
}

func TestLoadPolicyMissingFileIsAnError(t *testing.T) {
	if _, err := LoadPolicy(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("a policy path that does not exist must fail loudly, not fall back silently")
	}
}

// Absent means false in a file: a node handed a policy is a node that wants to
// be the authority. Getting this backwards would reopen the hole for everyone
// who wrote a policy without knowing about this field.
func TestWaiverDefaultsToFalseInAFile(t *testing.T) {
	p := mustLoadPolicy(t, "policy: 1\ndefault_effect: gate\n")
	if p.GraphWaiverAllowed() {
		t.Fatal("a policy file that says nothing about waivers must not grant them")
	}
	if !DefaultPolicy().GraphWaiverAllowed() {
		t.Fatal("the built-in default must grant them, or seeded graphs stop working")
	}
}

// The hash is what a ledger entry will cite as "the policy that allowed this",
// so it has to be stable across loads and different between documents.
func TestPolicyHashIsStableAndDistinguishing(t *testing.T) {
	body := "policy: 1\ndefault_effect: gate\n"
	a := mustLoadPolicy(t, body)
	b := mustLoadPolicy(t, body)
	if a.Hash() != b.Hash() {
		t.Errorf("same document hashed differently: %s vs %s", a.Hash(), b.Hash())
	}
	c := mustLoadPolicy(t, "policy: 1\ndefault_effect: deny\n")
	if a.Hash() == c.Hash() {
		t.Error("different documents share a hash")
	}
	if DefaultPolicy().Hash() == "" {
		t.Error("the built-in default has no hash; it must be auditable too")
	}
}

// --- wired into a session ---------------------------------------------------

// The end-to-end version of the hole: a hand-written graph that waives the gate
// on a motor skill must still be gated when the node runs under a policy.
func TestSessionIgnoresGraphWaiverUnderPolicy(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")
	pol := mustLoadPolicy(t, "policy: 1\ndefault_effect: gate\n")

	var toClient []channel.Envelope
	sess, err := NewSession("p1", graphInto("w", "motor.api.writer", GateNone),
		reg, testStore(t), "local", pol, nil, func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	sess.Route(clientData("p1"))

	if len(*delivered) != 0 {
		t.Fatalf("a graph waived the gate under policy: %d envelope(s) delivered", len(*delivered))
	}
	if len(toClient) != 1 || toClient[0].Kind != channel.KindConfirmRequest {
		t.Fatalf("want one confirm_request, got %+v", toClient)
	}
}

// A denied capability must stop the session being built at all, so the author
// hears about it when they wire it up rather than on the first message.
func TestSessionRefusedWhenPolicyDenies(t *testing.T) {
	reg := registry.New()
	liveSkill(t, reg, "acme/motor/payer", "motor", "motor.payments.send")
	pol := mustLoadPolicy(t, `
policy: 1
rules:
  - match: "motor.payments.*"
    decision: deny
`)
	_, err := NewSession("p2", graphInto("w", "motor.payments.send", GateNone),
		reg, testStore(t), "local", pol, nil, func([]byte, string) error { return nil }, testLogger())
	if err == nil {
		t.Fatal("a session wiring a denied capability was built anyway")
	}
}

// An allow rule delivers straight through — the counterpart to the deny test,
// and the thing that keeps a policy usable rather than merely safe.
func TestSessionDeliversWhenPolicyAllows(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/motor/tts", "motor", "motor.tts.speak")
	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
rules:
  - match: "motor.tts.speak"
    decision: allow
    reason: "speech is continuous; per-clause approval is unusable"
`)
	sess, err := NewSession("p3", graphInto("w", "motor.tts.speak", ""),
		reg, testStore(t), "local", pol, nil, func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("p3"))

	if len(*delivered) != 1 {
		t.Fatalf("an allowed effect got %d envelope(s); want 1", len(*delivered))
	}
}

// In published mode the graph never gets a vote, whatever the policy says
// about waivers elsewhere.
func TestPublishedModeIgnoresWaiverEvenWhenPolicyGrantsIt(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")
	pol := mustLoadPolicy(t, "policy: 1\ndefault_effect: gate\nallow_graph_waiver: true\n")

	var toClient []channel.Envelope
	sess, err := NewSession("p4", graphInto("w", "motor.api.writer", GateNone),
		reg, testStore(t), "published", pol, nil, func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("p4"))

	if len(*delivered) != 0 {
		t.Fatal("published mode honoured a graph-level waiver")
	}
	if len(toClient) != 1 || toClient[0].Kind != channel.KindConfirmRequest {
		t.Fatalf("want one confirm_request, got %+v", toClient)
	}
}

// A rate limit refuses the delivery and tells the client why, rather than
// dropping it silently.
func TestSessionEnforcesRateLimitAndExplains(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")
	pol := mustLoadPolicy(t, `
policy: 1
default_effect: allow
rules:
  - match: "motor.api.*"
    decision: allow
    limit: { per_minute: 1 }
`)
	var toClient []channel.Envelope
	sess, err := NewSession("p5", graphInto("w", "motor.api.writer", ""),
		reg, testStore(t), "local", pol, nil, func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	first := clientData("p5")
	sess.Route(first)
	second := clientData("p5")
	second.Idem = "p5:2" // a distinct message, or dedup would eat it
	sess.Route(second)

	if len(*delivered) != 1 {
		t.Fatalf("delivered %d envelope(s) against a 1/min limit; want 1", len(*delivered))
	}
	var sawError bool
	for _, env := range toClient {
		if env.Kind == channel.KindError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("a rate-limited delivery was dropped without telling the client")
	}
}
