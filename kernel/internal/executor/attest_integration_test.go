package executor

import (
	"encoding/json"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
)

// C5 end to end, through the real executor path: a cognitive skill attests,
// a motor skill downstream produces an effect, and the sealed entry cites
// what argued for it.
//
// The unit tests in internal/ledger prove the crypto. These prove the
// *wiring* — that an attestation on an envelope actually reaches the ledger
// entry of an effect two hops later, which is the claim the whole feature
// rests on and the one most likely to break silently in a refactor.

const attestRecord = `{"engine":"llama.cpp","model":"Qwen/Qwen2.5-1.5B-Instruct-GGUF",` +
	`"model_revision":"f1d2d2f924e986ac86fdf7b36c94bcdf32beec15","quantization":"Q4_K_M",` +
	`"params":{"temperature":0.7,"seed":42}}`

// chainGraph wires client -> thinker (cognitive) -> writer (motor), the
// shape every real "a model decided, then something happened" graph has.
func chainGraph(gate string) *Graph {
	return &Graph{
		IR: IRMajor, GraphID: "attest-chain",
		Nodes: []Node{
			{Ref: "thinker", Resolve: "cognitive.llm.chat"},
			{Ref: "writer", Resolve: "motor.api.writer"},
		},
		Edges: []Edge{
			{From: "client.text_out", To: "thinker.text_in"},
			{From: "thinker.text_out", To: "writer.text_in", Gate: gate},
		},
	}
}

// registerThinker registers a cognitive skill whose reply carries a C5
// attestation, mimicking what skills/llm-chat now emits on its final chunk.
func registerThinker(t *testing.T, reg *registry.Registry, sess func() *Session, attest json.RawMessage) {
	t.Helper()
	m := registry.Manifest{
		ID: "acme/cognitive/thinker", Version: "2.0.0", Protocol: "1",
		Name: "Thinker", Description: "reasons",
		Capability: "cognitive.llm.chat", Type: "cognitive", Format: "source",
	}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	m.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}

	reg.Register(m.ID, &registry.Live{
		Manifest: m,
		Send: func(raw []byte, _ string) error {
			var in channel.Envelope
			if err := json.Unmarshal(raw, &in); err != nil {
				return err
			}
			if in.Kind != channel.KindData {
				return nil
			}
			// Reply the way a real skill does: emit back into the session,
			// carrying the attestation on the envelope.
			sess().Route(channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: in.ID,
				Session: in.Session, Node: "thinker", Port: "text_out", Seq: 1,
				Idem: in.Idem + ":reply", Schema: "std/text@1", Kind: channel.KindData,
				Payload: json.RawMessage(`{"text":"pay the invoice","final":true}`),
				Attest:  attest,
			})
			return nil
		},
	})
}

func TestEffectCitesTheInferenceThatCausedIt(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var sess *Session
	registerThinker(t, reg, func() *Session { return sess }, json.RawMessage(attestRecord))

	pol := mustLoadPolicy(t, `
policy: 1
default_effect: gate
rules:
  - match: "motor.api.writer"
    decision: allow
`)
	var err error
	sess, err = NewSession("sess-c5-1", chainGraph(""), reg, st, "local", pol, ldg, "",
		func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	sess.Route(clientData("sess-c5-1"))

	if len(*delivered) != 1 {
		t.Fatalf("got %d deliveries into the motor skill, want 1", len(*delivered))
	}
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(entries))
	}

	// The claim: the effect names the inference upstream of it.
	if len(entries[0].Inference) != 1 {
		t.Fatalf("sealed effect cites %d inferences, want 1: %+v",
			len(entries[0].Inference), entries[0])
	}
	wantHash := ledger.AttestationHash([]byte(attestRecord))
	if entries[0].Inference[0] != wantHash {
		t.Fatalf("effect cites %s, want %s", entries[0].Inference[0], wantHash)
	}

	// And the record it names is retrievable and intact.
	a, err := ledger.LoadAttestation(st, wantHash)
	if err != nil {
		t.Fatalf("the cited attestation could not be loaded: %v", err)
	}
	if a.Model != "Qwen/Qwen2.5-1.5B-Instruct-GGUF" || a.Quantization != "Q4_K_M" {
		t.Fatalf("attestation round-tripped wrong: %+v", a)
	}
}

// A receipt built from this session must verify standalone and carry the
// attestation — the full artifact an auditor would actually be handed.
func TestReceiptFromARealSessionCarriesTheInference(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var sess *Session
	registerThinker(t, reg, func() *Session { return sess }, json.RawMessage(attestRecord))

	pol := mustLoadPolicy(t, "policy: 1\ndefault_effect: allow\n")
	var err error
	sess, err = NewSession("sess-c5-2", chainGraph(""), reg, st, "local", pol, ldg, "",
		func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-c5-2"))

	receiptHash := (*delivered)[0].Receipt
	if receiptHash == "" {
		t.Fatal("the delivered envelope carries no receipt")
	}
	if err := ldg.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	r, err := ledger.BuildReceipt(st, receiptHash)
	if err != nil {
		t.Fatalf("BuildReceipt: %v", err)
	}
	// Round-trip through JSON, as a real hand-off would.
	blob, _ := json.Marshal(r)
	var portable ledger.Receipt
	if err := json.Unmarshal(blob, &portable); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rep := ledger.VerifyReceipt(portable)
	if !rep.Sound() {
		t.Fatalf("receipt from a real session did not verify: %+v", rep)
	}
	if rep.AttestationsResolved != 1 {
		t.Fatalf("receipt resolved %d/%d attestations, want 1/1",
			rep.AttestationsResolved, rep.AttestationsCited)
	}
}

// A denied gate seals too, and cites the same inference: "which model kept
// proposing the payment a human kept refusing" has to be answerable.
func TestDeniedEffectAlsoCitesTheInference(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	_ = liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var sess *Session
	var toClient []channel.Envelope
	registerThinker(t, reg, func() *Session { return sess }, json.RawMessage(attestRecord))

	var err error
	sess, err = NewSession("sess-c5-3", chainGraph(GateHumanApproval), reg, st, "local",
		DefaultPolicy(), ldg, "",
		func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-c5-3"))

	var confirm channel.Envelope
	for _, e := range toClient {
		if e.Kind == channel.KindConfirmRequest {
			confirm = e
		}
	}
	if confirm.ID == "" {
		t.Fatal("no confirm_request reached the client")
	}
	payload, _ := json.Marshal(map[string]bool{"approve": false})
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: confirm.ID,
		Session: "sess-c5-3", Kind: channel.KindConfirmResponse, Payload: payload,
	})

	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1 (the refusal)", len(entries))
	}
	if entries[0].Outcome != "denied" {
		t.Fatalf("outcome = %q, want denied", entries[0].Outcome)
	}
	if len(entries[0].Inference) != 1 {
		t.Fatalf("a refused effect cites %d inferences, want 1", len(entries[0].Inference))
	}
}

// A malformed attestation must not break routing: the output still flows and
// the effect is still sealed, just citing nothing.
func TestMalformedAttestationDoesNotBreakTheChain(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	delivered := liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	var sess *Session
	// No engine, no model: rejected by ParseAttestation.
	registerThinker(t, reg, func() *Session { return sess }, json.RawMessage(`{"nonsense":true}`))

	pol := mustLoadPolicy(t, "policy: 1\ndefault_effect: allow\n")
	var err error
	sess, err = NewSession("sess-c5-4", chainGraph(""), reg, st, "local", pol, ldg, "",
		func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-c5-4"))

	if len(*delivered) != 1 {
		t.Fatalf("a malformed attestation stopped the chain: %d deliveries", len(*delivered))
	}
	entries := allEntries(t, st)
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(entries))
	}
	if len(entries[0].Inference) != 0 {
		t.Fatalf("a rejected attestation was still cited: %+v", entries[0].Inference)
	}
}

// An effect no inference contributed to cites nothing, and its entry
// serializes exactly as it did before C5 — so pre-C5 entries rehash unchanged.
func TestEffectWithNoInferenceIsUnchanged(t *testing.T) {
	reg := registry.New()
	id, m := motorManifestWithCompensation()
	_ = liveSkillWithManifest(t, reg, id, m)
	ldg, st := testLedgerForSession(t)

	pol := mustLoadPolicy(t, "policy: 1\ndefault_effect: allow\n")
	sess, err := NewSession("sess-c5-5", graphInto("w", "motor.api.writer", ""),
		reg, st, "local", pol, ldg, "", func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.Route(clientData("sess-c5-5"))

	raw, err := st.LedgerEntries(1, 0)
	if err != nil || len(raw) != 1 {
		t.Fatalf("LedgerEntries: %v (%d entries)", err, len(raw))
	}
	if json.Valid(raw[0]) && containsKey(raw[0], "inference") {
		t.Fatalf("an effect with no inference emitted an `inference` key: %s", raw[0])
	}
}

func containsKey(raw []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
