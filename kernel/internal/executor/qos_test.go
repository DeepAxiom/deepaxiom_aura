package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
)

// qosGraph wires client -> skill -> client with a declared class on the edge
// carrying results back, which is the direction voice actually streams.
func qosGraph(qos string) *Graph {
	g := &Graph{IR: IRMajor, GraphID: "q"}
	g.Origin.Kind = "declared"
	g.Nodes = []Node{{Ref: "s", Resolve: "logical.a"}}
	g.Edges = []Edge{
		{From: "client.text_out", To: "s.text_in"},
		{From: "s.text_out", To: "client.text_in", QoS: qos},
	}
	return g
}

type sent struct {
	env channel.Envelope
	qos string
}

func qosSession(t *testing.T, qos string, st *store.Store) (*Session, *[]sent, *[]channel.Envelope) {
	t.Helper()
	reg := registry.New()
	toSkill := liveSkill(t, reg, "acme/logical/a", "logical", "logical.a")

	var toClient []sent
	sess, err := NewSession("q1", qosGraph(qos), reg, st, "local", DefaultPolicy(), nil,
		func(raw []byte, q string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			toClient = append(toClient, sent{env: env, qos: q})
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess, &toClient, toSkill
}

// The value the IR declares has to reach the send path. It was parsed and
// dropped before this, so every edge behaved as reliable whatever it said.
func TestDeclaredQoSReachesTheSendPath(t *testing.T) {
	for _, qos := range []string{channel.QoSRealtime, channel.QoSReliable} {
		t.Run(qos, func(t *testing.T) {
			sess, toClient, toSkill := qosSession(t, qos, testStore(t))
			sess.Route(clientData("q1"))
			if len(*toSkill) != 1 {
				t.Fatalf("skill got %d envelope(s), want 1", len(*toSkill))
			}
			sess.Route(emission((*toSkill)[0], "s", "text_out", "reply"))

			if len(*toClient) != 1 {
				t.Fatalf("client got %d envelope(s), want 1", len(*toClient))
			}
			if (*toClient)[0].qos != qos {
				t.Fatalf("delivered on the %q lane, want %q", (*toClient)[0].qos, qos)
			}
		})
	}
}

// An edge with no declared class is reliable — C3 rule 4's default, and the
// behaviour every existing graph already relies on.
func TestUndeclaredQoSDefaultsToReliable(t *testing.T) {
	sess, toClient, toSkill := qosSession(t, "", testStore(t))
	sess.Route(clientData("q1"))
	sess.Route(emission((*toSkill)[0], "s", "text_out", "reply"))

	if (*toClient)[0].qos != channel.QoSReliable {
		t.Fatalf("undeclared edge delivered on %q, want reliable", (*toClient)[0].qos)
	}
}

// Only `data` may be dropped. A done, an error or a status is terminal or
// explanatory — losing one leaves a consumer waiting forever for something
// that already happened.
func TestTerminalKindsIgnoreARealtimeEdge(t *testing.T) {
	sess, toClient, toSkill := qosSession(t, channel.QoSRealtime, testStore(t))
	sess.Route(clientData("q1"))
	incoming := (*toSkill)[0]

	for _, kind := range []string{channel.KindDone, channel.KindError, channel.KindStatus} {
		out := emission(incoming, "s", "text_out", "x")
		out.Kind = kind
		out.ID = channel.NewID()
		out.Idem = channel.NewID()
		sess.Route(out)
	}

	if len(*toClient) != 3 {
		t.Fatalf("client got %d envelope(s), want 3", len(*toClient))
	}
	for _, s := range *toClient {
		if s.qos != channel.QoSReliable {
			t.Fatalf("%s went out on the %q lane; terminal kinds must not be droppable",
				s.env.Kind, s.qos)
		}
	}
}

// A minute of speech is megabytes of base64 in SQLite and a permanent
// recording of someone talking. The causal chain is what `aura why` needs;
// the samples are not.
func TestRealtimePayloadsAreElidedFromTheEventLog(t *testing.T) {
	st := testStore(t)
	sess, _, toSkill := qosSession(t, channel.QoSRealtime, st)
	sess.Route(clientData("q1"))
	sess.Route(emission((*toSkill)[0], "s", "text_out", "SENSITIVE-SPEECH-SAMPLES"))

	events, _, err := st.SessionEvents("q1", 0)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}

	var elided *channel.Envelope
	for i := range events {
		var env channel.Envelope
		_ = json.Unmarshal(events[i], &env)
		if env.Node == ClientRef && env.Port == "text_in" {
			elided = &env
		}
		if strings.Contains(string(events[i]), "SENSITIVE-SPEECH-SAMPLES") &&
			env.Node == ClientRef {
			t.Fatal("a realtime payload was written to the event log verbatim")
		}
	}
	if elided == nil {
		t.Fatal("the realtime delivery is missing from the log entirely")
	}

	// The envelope itself must still be there in full: eliding the payload
	// must not cost the causal chain, or `aura why` stops working.
	if elided.ID == "" || elided.CauseID == "" || elided.Schema == "" {
		t.Fatalf("elision damaged the envelope header: %+v", elided)
	}
	var body map[string]any
	_ = json.Unmarshal(elided.Payload, &body)
	if body["elided"] != true {
		t.Fatalf("want the payload replaced by a description of itself, got %v", body)
	}
	if body["bytes"] == nil || body["sha256"] == nil {
		t.Fatalf("an elided payload should say how big it was and hash to what: %v", body)
	}
}

// Elision keys off the edge's declared class, not the payload's schema: a
// graph that declares an audio edge reliable is asking for a recording.
func TestReliablePayloadsAreLoggedInFull(t *testing.T) {
	st := testStore(t)
	sess, _, toSkill := qosSession(t, channel.QoSReliable, st)
	sess.Route(clientData("q1"))
	sess.Route(emission((*toSkill)[0], "s", "text_out", "KEEP-THIS-VERBATIM"))

	events, _, err := st.SessionEvents("q1", 0)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	found := false
	for _, raw := range events {
		if strings.Contains(string(raw), "KEEP-THIS-VERBATIM") {
			found = true
		}
	}
	if !found {
		t.Fatal("a reliable payload should be persisted in full")
	}
}
