package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
)

// Speculative graph execution: the claim is that downstream work starts on a
// partial and is discarded, safely, when the partial turns out wrong — and
// that "safely" is guaranteed by the effect type system rather than by
// anyone's judgement.

// streamingGraph wires client -> producer -> consumer, with the second edge
// speculative.
func streamingGraph(consumerCapability string, speculative bool) *Graph {
	return &Graph{
		IR: IRMajor, GraphID: "spec-graph",
		Nodes: []Node{
			{Ref: "producer", Resolve: "cognitive.llm.chat"},
			{Ref: "consumer", Resolve: consumerCapability},
		},
		Edges: []Edge{
			{From: "client.text_out", To: "producer.text_in"},
			{From: "producer.text_out", To: "consumer.text_in", Speculative: speculative},
		},
	}
}

// recordingSkill registers a skill of the given type and returns what it was
// delivered.
func recordingSkill(t *testing.T, reg *registry.Registry, id, capability, skillType string) *[]channel.Envelope {
	t.Helper()
	m := registry.Manifest{
		ID: id, Version: "1.0.0", Protocol: "1",
		Name: id, Description: "test skill",
		Capability: capability, Type: skillType, Format: "source",
	}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	m.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}
	return liveSkillWithManifest(t, reg, id, m)
}

// emitPartials drives one turn: a client message, then a sequence of
// std/text@1 deltas streamed back by the producer, then its final chunk.
//
// The producer's replies name, as their cause, the envelope the kernel
// actually delivered to it — not the client's message. That is what a real
// skill does (the SDK sets cause_id from the incoming envelope), and it is
// what lets the kernel map the reply back to its causal root. Getting this
// wrong in a test makes speculation silently not happen, because a chain with
// no known root is never speculated on.
func emitPartials(t *testing.T, s *Session, producerGot *[]channel.Envelope, deltas []string, final string) {
	t.Helper()
	root := clientData(s.ID)
	s.Route(root)

	delivered := dataOnly(*producerGot)
	if len(delivered) == 0 {
		t.Fatal("the client message never reached the producer")
	}
	cause := delivered[0]

	emit := func(seq int, text string, isFinal bool) {
		payload, _ := json.Marshal(map[string]any{"text": text, "final": isFinal})
		s.Route(channel.Envelope{
			V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: cause.ID,
			Session: s.ID, Node: "producer", Port: "text_out", Seq: uint64(seq),
			Idem: fmt.Sprintf("%s:reply:%d", cause.Idem, seq), Schema: "std/text@1",
			Kind: channel.KindData, Payload: payload,
		})
	}
	for i, delta := range deltas {
		emit(i+1, delta, false)
	}
	emit(len(deltas)+1, final, true)
}

func specSession(t *testing.T, g *Graph, reg *registry.Registry, pol *Policy) *Session {
	t.Helper()
	if pol == nil {
		pol = mustLoadPolicy(t, "policy: 1\ndefault_effect: allow\n")
	}
	s, err := NewSession("sess-spec", g, reg, testStore(t), "local", pol, nil, "",
		func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

// THE INVARIANT. An edge into a motor skill may never speculate, and the
// refusal happens at wiring — before a session exists, before anything runs.
//
// This is what makes speculation safe by construction rather than by review:
// C1's type system already says which skills act on the world.
func TestSpeculationIsRefusedOnAMotorEdge(t *testing.T) {
	reg := registry.New()
	recordingSkill(t, reg, "acme/cognitive/producer", "cognitive.llm.chat", "cognitive")
	recordingSkill(t, reg, "acme/motor/writer", "motor.api.writer", "motor")

	_, err := NewSession("sess-refused", streamingGraph("motor.api.writer", true),
		reg, testStore(t), "local",
		mustLoadPolicy(t, "policy: 1\ndefault_effect: allow\n"), nil, "",
		func([]byte, string) error { return nil }, testLogger())
	if err == nil {
		t.Fatal("an edge into a motor skill was allowed to speculate; a discarded " +
			"effect is not discarded")
	}
	if !strings.Contains(err.Error(), "motor") || !strings.Contains(err.Error(), "speculative") {
		t.Fatalf("the refusal should name both the edge's request and the skill type, got: %v", err)
	}
}

// The same edge into a non-motor skill is fine, which is the other half of the
// invariant being useful rather than merely restrictive.
func TestSpeculationIsAllowedOnANonMotorEdge(t *testing.T) {
	reg := registry.New()
	recordingSkill(t, reg, "acme/cognitive/producer", "cognitive.llm.chat", "cognitive")
	recordingSkill(t, reg, "acme/logical/summariser", "logical.summarise", "logical")

	if _, err := NewSession("sess-ok", streamingGraph("logical.summarise", true),
		reg, testStore(t), "local",
		mustLoadPolicy(t, "policy: 1\ndefault_effect: allow\n"), nil, "",
		func([]byte, string) error { return nil }, testLogger()); err != nil {
		t.Fatalf("a speculative edge into a logical skill was refused: %v", err)
	}
}

// A hit: the accumulated partials equal the final output, so the downstream
// already has the right value and the redundant final delivery is suppressed.
func TestSpeculationHitSuppressesTheRedundantDelivery(t *testing.T) {
	reg := registry.New()
	producerGot := recordingSkill(t, reg, "acme/cognitive/producer", "cognitive.llm.chat", "cognitive")
	got := recordingSkill(t, reg, "acme/logical/summariser", "logical.summarise", "logical")

	s := specSession(t, streamingGraph("logical.summarise", true), reg, nil)
	// "pay " + "the " + "invoice" accumulates to exactly the final value.
	emitPartials(t, s, producerGot, []string{"pay ", "the ", "invoice"}, "")

	data := dataOnly(*got)
	if len(data) != 3 {
		t.Fatalf("expected 3 speculative deliveries, got %d", len(data))
	}
	last := textOf(t, data[len(data)-1])
	if last != "pay the invoice" {
		t.Fatalf("last speculative delivery carried %q, want the accumulated value", last)
	}
	stats := s.SpeculationStats()
	if stats.Hits != 1 || stats.Misses != 0 {
		t.Fatalf("expected 1 hit and 0 misses, got %+v", stats)
	}
	if stats.Attempts != 3 {
		t.Fatalf("expected 3 speculative attempts, got %d", stats.Attempts)
	}
}

// A miss: the producer's final output differs from what was speculated on, so
// the speculative work is cancelled and the truth delivered.
func TestSpeculationMissCancelsAndRedelivers(t *testing.T) {
	reg := registry.New()
	producerGot := recordingSkill(t, reg, "acme/cognitive/producer", "cognitive.llm.chat", "cognitive")
	got := recordingSkill(t, reg, "acme/logical/summariser", "logical.summarise", "logical")

	s := specSession(t, streamingGraph("logical.summarise", true), reg, nil)
	// The final delta changes the meaning entirely — the classic reason
	// speculating on a prefix is a guess and not a shortcut.
	emitPartials(t, s, producerGot, []string{"pay ", "the "}, " do NOT pay")

	stats := s.SpeculationStats()
	if stats.Misses != 1 || stats.Hits != 0 {
		t.Fatalf("expected 1 miss and 0 hits, got %+v", stats)
	}

	var cancels int
	for _, env := range *got {
		if env.Kind == channel.KindCancel {
			cancels++
		}
	}
	if cancels == 0 {
		t.Fatal("a speculation miss did not ask the downstream skill to abandon its work")
	}

	data := dataOnly(*got)
	last := textOf(t, data[len(data)-1])
	if last != "pay the  do NOT pay" {
		t.Fatalf("after a miss the real value should be delivered, got %q", last)
	}
}

// After a miss, anything the speculative chain emits must go nowhere — the
// same guarantee `cancel` gives, reused rather than reinvented.
func TestSpeculativeOutputIsSuppressedAfterAMiss(t *testing.T) {
	reg := registry.New()
	producerGot := recordingSkill(t, reg, "acme/cognitive/producer", "cognitive.llm.chat", "cognitive")
	recordingSkill(t, reg, "acme/logical/summariser", "logical.summarise", "logical")

	s := specSession(t, streamingGraph("logical.summarise", true), reg, nil)
	emitPartials(t, s, producerGot, []string{"a"}, "b")

	if s.SpeculationStats().Misses != 1 {
		t.Fatalf("precondition: expected a miss, got %+v", s.SpeculationStats())
	}
	// The kernel marked the superseded envelope ids cancelled; a late emission
	// naming one of them as its cause is dropped.
	if s.cancelled == nil {
		t.Fatal("no cancellation state after a miss")
	}
}

// A graph that never asks to speculate behaves exactly as it did before the
// feature existed.
func TestNonSpeculativeEdgeIsUnchanged(t *testing.T) {
	reg := registry.New()
	producerGot := recordingSkill(t, reg, "acme/cognitive/producer", "cognitive.llm.chat", "cognitive")
	got := recordingSkill(t, reg, "acme/logical/summariser", "logical.summarise", "logical")

	s := specSession(t, streamingGraph("logical.summarise", false), reg, nil)
	emitPartials(t, s, producerGot, []string{"a", "b"}, "c")

	data := dataOnly(*got)
	if len(data) != 3 {
		t.Fatalf("expected every partial forwarded verbatim, got %d deliveries", len(data))
	}
	// Verbatim: deltas, not accumulated.
	if textOf(t, data[0]) != "a" || textOf(t, data[2]) != "c" {
		t.Fatalf("a non-speculative edge accumulated payloads: %q .. %q",
			textOf(t, data[0]), textOf(t, data[2]))
	}
	if s.SpeculationStats().Attempts != 0 {
		t.Fatal("a non-speculative edge attempted a speculation")
	}
}

// Node policy can turn speculation off everywhere without editing graphs.
func TestPolicyCanDenySpeculation(t *testing.T) {
	reg := registry.New()
	producerGot := recordingSkill(t, reg, "acme/cognitive/producer", "cognitive.llm.chat", "cognitive")
	got := recordingSkill(t, reg, "acme/logical/summariser", "logical.summarise", "logical")

	pol := mustLoadPolicy(t, "policy: 1\ndefault_effect: allow\nspeculation: deny\n")
	s := specSession(t, streamingGraph("logical.summarise", true), reg, pol)
	emitPartials(t, s, producerGot, []string{"a", "b"}, "c")

	if s.SpeculationStats().Attempts != 0 {
		t.Fatalf("policy denied speculation but %d attempts were made",
			s.SpeculationStats().Attempts)
	}
	if len(dataOnly(*got)) != 3 {
		t.Fatal("denying speculation should fall back to ordinary forwarding")
	}
}

// std/transcript@1 replaces rather than appends. Folding it as a delta would
// make every speculation a miss — or worse, a hit on concatenated garbage.
func TestAccumulationFollowsSchemaSemantics(t *testing.T) {
	t.Run("text appends", func(t *testing.T) {
		acc := accumulate("std/text@1",
			json.RawMessage(`{"text":"hello "}`), json.RawMessage(`{"text":"world","final":true}`))
		if textFieldOf(t, acc) != "hello world" {
			t.Fatalf("std/text@1 must concatenate, got %q", textFieldOf(t, acc))
		}
	})
	t.Run("transcript replaces", func(t *testing.T) {
		acc := accumulate("std/transcript@1",
			json.RawMessage(`{"text":"hell","final":false}`),
			json.RawMessage(`{"text":"hello world","final":true}`))
		if textFieldOf(t, acc) != "hello world" {
			t.Fatalf("std/transcript@1 must replace, got %q", textFieldOf(t, acc))
		}
	})
	t.Run("unknown schema replaces", func(t *testing.T) {
		acc := accumulate("acme/thing@1",
			json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`))
		if string(acc) != `{"a":2}` {
			t.Fatalf("an unknown schema must replace (the safe default), got %s", acc)
		}
	})
}

func TestIsFinalReadsThePayloadNotTheKind(t *testing.T) {
	cases := map[string]bool{
		`{"text":"a","final":true}`:  true,
		`{"text":"a","final":false}`: false,
		`{"text":"a"}`:               false,
		``:                           false,
		`not json`:                   false,
	}
	for payload, want := range cases {
		if got := isFinal(json.RawMessage(payload)); got != want {
			t.Errorf("isFinal(%q) = %v, want %v", payload, got, want)
		}
	}
}

// --- helpers -----------------------------------------------------------------

func dataOnly(envs []channel.Envelope) []channel.Envelope {
	var out []channel.Envelope
	for _, e := range envs {
		if e.Kind == channel.KindData {
			out = append(out, e)
		}
	}
	return out
}

func textOf(t *testing.T, env channel.Envelope) string {
	t.Helper()
	return textFieldOf(t, env.Payload)
}

func textFieldOf(t *testing.T, payload json.RawMessage) string {
	t.Helper()
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("payload %s: %v", payload, err)
	}
	return p.Text
}
