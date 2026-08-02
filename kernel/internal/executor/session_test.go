package executor

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// liveSkill registers a skill of the given type/capability and captures
// everything the kernel delivers to it.
func liveSkill(t *testing.T, reg *registry.Registry, id, typ, capability string) *[]channel.Envelope {
	t.Helper()
	got := &[]channel.Envelope{}
	m := registry.Manifest{
		ID: id, Version: "1.0.0", Protocol: "1",
		Name: id, Description: "test skill",
		Capability: capability, Type: typ, Format: "source",
	}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	m.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}
	reg.Register(id, &registry.Live{
		Manifest: m,
		Send: func(raw []byte, _ string) error {
			var env channel.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				return err
			}
			*got = append(*got, env)
			return nil
		},
	})
	return got
}

func graphInto(ref, capability, gate string) *Graph {
	g := &Graph{IR: IRMajor, GraphID: "t"}
	g.Origin.Kind = "declared"
	g.Nodes = []Node{{Ref: ref, Resolve: capability}}
	g.Edges = []Edge{
		{From: "client.text_out", To: ref + ".text_in", Gate: gate},
		{From: ref + ".text_out", To: "client.text_in"},
	}
	return g
}

func clientData(session string) channel.Envelope {
	payload, _ := json.Marshal(map[string]any{"text": "do the thing", "final": true})
	return channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: session,
		Node: ClientRef, Port: "text_out", Seq: 1, Idem: session + ":1",
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	}
}

// A hand-declared graph that writes into a motor.* skill with no gate must
// still be gated. Before the invariant moved into the kernel, the message was
// delivered straight through and the write happened with no human approval.
func TestMotorEdgeIsGatedWhenGraphDeclaresNoGate(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")

	var toClient []channel.Envelope
	sess, err := NewSession("s1", graphInto("w", "motor.api.writer", ""),
		reg, testStore(t), "local", DefaultPolicy(), nil, func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	sess.Route(clientData("s1"))

	if len(*delivered) != 0 {
		t.Fatalf("motor skill received %d envelope(s) without approval; want 0", len(*delivered))
	}
	if len(toClient) != 1 || toClient[0].Kind != channel.KindConfirmRequest {
		t.Fatalf("want one confirm_request to the client, got %+v", toClient)
	}
}

// The same graph pointed at a non-motor skill must NOT be gated: the
// invariant has to be narrow, or every graph becomes unusable.
func TestNonMotorEdgeIsNotGated(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")

	sess, err := NewSession("s2", graphInto("e", "logical.echo", ""),
		reg, testStore(t), "local", DefaultPolicy(), nil, func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	sess.Route(clientData("s2"))

	if len(*delivered) != 1 {
		t.Fatalf("logical skill got %d envelope(s); want 1 delivered straight through", len(*delivered))
	}
	if (*delivered)[0].Kind != channel.KindData {
		t.Fatalf("want a data envelope, got %q", (*delivered)[0].Kind)
	}
}

// An explicit gate in the IR must be preserved, not doubled or replaced.
func TestExplicitGateOnMotorEdgeIsPreserved(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")

	var toClient []channel.Envelope
	sess, err := NewSession("s3", graphInto("w", "motor.api.writer", GateHumanApproval),
		reg, testStore(t), "local", DefaultPolicy(), nil, func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	sess.Route(clientData("s3"))

	if len(*delivered) != 0 {
		t.Fatalf("motor skill received %d envelope(s) before approval; want 0", len(*delivered))
	}
	if len(toClient) != 1 {
		t.Fatalf("want exactly one confirm_request (not doubled), got %d", len(toClient))
	}
}

// The invariant is aimed at omission, not at the action. An author who writes
// the opt-out down has considered it, and some motor skills are unusable
// otherwise — speech synthesis is `motor`, and a voice assistant that asked
// permission before every clause would not be one.
func TestExplicitGateNoneOptsOutOfTheMotorInvariant(t *testing.T) {
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")

	var toClient []channel.Envelope
	sess, err := NewSession("s6", graphInto("w", "motor.api.writer", GateNone),
		reg, testStore(t), "local", DefaultPolicy(), nil, func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	sess.Route(clientData("s6"))

	if len(*delivered) != 1 {
		t.Fatalf("motor skill got %d envelope(s); an explicit opt-out should deliver", len(*delivered))
	}
	for _, env := range toClient {
		if env.Kind == channel.KindConfirmRequest {
			t.Fatal("an explicitly ungated edge still asked for approval")
		}
	}
}

// The distinction has to survive the parser too, or `gate: none` would be
// rejected as an unknown gate before it ever reached the invariant.
func TestGraphParserAcceptsBothGatesAndRejectsOthers(t *testing.T) {
	for _, gate := range []string{GateHumanApproval, GateNone, ""} {
		g := graphInto("w", "motor.api.writer", gate)
		raw, _ := json.Marshal(g)
		if _, err := ParseGraph(raw); err != nil {
			t.Fatalf("gate %q should parse: %v", gate, err)
		}
	}
	g := graphInto("w", "motor.api.writer", "maybe-later")
	raw, _ := json.Marshal(g)
	if _, err := ParseGraph(raw); err == nil {
		t.Fatal("an unknown gate must be refused rather than ignored")
	}
}

// In published mode an ungated motor edge is an authoring error, not
// something to paper over: the session is refused so the author hears about it.
func TestPublishedModeRefusesUngatedMotorEdge(t *testing.T) {
	reg := registry.New()
	liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")

	_, err := NewSession("s4", graphInto("w", "motor.api.writer", ""),
		reg, testStore(t), "published", DefaultPolicy(), nil, func([]byte, string) error { return nil }, testLogger())
	if err == nil {
		t.Fatal("want published mode to refuse an ungated motor edge, got nil error")
	}
}

// Approving a held motor message delivers it; denying it does not.
func TestGateApprovalDeliversAndDenialDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		approve     bool
		wantDeliver int
	}{
		{"approve delivers", true, 1},
		{"deny does not deliver", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := registry.New()
			delivered := liveSkill(t, reg, "acme/motor/writer", "motor", "motor.api.writer")

			var toClient []channel.Envelope
			sess, err := NewSession("s", graphInto("w", "motor.api.writer", ""),
				reg, testStore(t), "local", DefaultPolicy(), nil, func(raw []byte, _ string) error {
					var env channel.Envelope
					_ = json.Unmarshal(raw, &env)
					toClient = append(toClient, env)
					return nil
				}, testLogger())
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}

			sess.Route(clientData("s"))
			if len(toClient) == 0 {
				t.Fatal("no confirm_request emitted")
			}
			req := toClient[0]

			payload, _ := json.Marshal(map[string]bool{"approve": tc.approve})
			sess.Route(channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: req.ID,
				Session: "s", Kind: channel.KindConfirmResponse, Payload: payload,
			})

			if len(*delivered) != tc.wantDeliver {
				t.Fatalf("delivered %d envelope(s); want %d", len(*delivered), tc.wantDeliver)
			}
		})
	}
}
