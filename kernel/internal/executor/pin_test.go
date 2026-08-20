package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
)

// pinnedSession wires client -> ref -> client and pins `ref`.
func pinnedSession(t *testing.T, mode string, pins map[string]Pin) (
	*Session, *[]channel.Envelope, *[]channel.Envelope, error,
) {
	t.Helper()
	reg := registry.New()
	delivered := liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")

	toClient := &[]channel.Envelope{}
	sess, err := NewSession("p1", graphInto("e", "logical.echo", ""),
		reg, testStore(t), mode, DefaultPolicy(), nil, "", func(raw []byte, _ string) error {
			var env channel.Envelope
			_ = json.Unmarshal(raw, &env)
			*toClient = append(*toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess, delivered, toClient, sess.SetPins(pins, mode)
}

func textPin(text string) Pin {
	payload, _ := json.Marshal(map[string]any{"text": text, "final": true})
	return Pin{Port: "text_out", Payload: payload}
}

// The whole point: the skill does not run.
func TestPinnedNodeIsNeverDispatchedTo(t *testing.T) {
	sess, delivered, toClient, err := pinnedSession(t, "local", map[string]Pin{"e": textPin("canned")})
	if err != nil {
		t.Fatalf("SetPins: %v", err)
	}
	sess.Route(clientData("p1"))

	if len(*delivered) != 0 {
		t.Fatalf("pinned skill was dispatched to %d time(s); the point is that it does not run", len(*delivered))
	}

	var data []channel.Envelope
	for _, env := range *toClient {
		if env.Kind == channel.KindData {
			data = append(data, env)
		}
	}
	if len(data) != 1 {
		t.Fatalf("want exactly one answer to the client, got %d", len(data))
	}
	if !strings.Contains(string(data[0].Payload), "canned") {
		t.Fatalf("the client got %s, not the pinned payload", data[0].Payload)
	}
}

// A pinned output that the record presents as the skill's own work would be a
// forged effect ledger. The chain has to say a human supplied it.
func TestPinnedOutputIsAnnouncedOnTheChain(t *testing.T) {
	sess, _, toClient, err := pinnedSession(t, "local", map[string]Pin{"e": textPin("canned")})
	if err != nil {
		t.Fatalf("SetPins: %v", err)
	}
	sess.Route(clientData("p1"))

	var note *channel.Envelope
	for i, env := range *toClient {
		if env.Kind == channel.KindStatus && strings.Contains(string(env.Payload), `"pinned"`) {
			note = &(*toClient)[i]
			break
		}
	}
	if note == nil {
		t.Fatal("no status announcing the pin; the log would present this as the skill's own output")
	}
	if !strings.Contains(string(note.Payload), `"node":"e"`) {
		t.Fatalf("the announcement must name the node it stood in for, got %s", note.Payload)
	}
}

// The announcement has to reach the durable log, not only the socket: `aura
// why` reads the log, and a live client that was not watching is the normal case.
func TestPinnedAnnouncementIsInTheCausalLog(t *testing.T) {
	st := testStore(t)
	reg := registry.New()
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	sess, err := NewSession("p2", graphInto("e", "logical.echo", ""),
		reg, st, "local", DefaultPolicy(), nil, "", func([]byte, string) error { return nil }, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := sess.SetPins(map[string]Pin{"e": textPin("canned")}, "local"); err != nil {
		t.Fatalf("SetPins: %v", err)
	}
	sess.Route(clientData("p2"))

	events, _, err := st.SessionEvents("p2", 100)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	var found bool
	for _, raw := range events {
		if strings.Contains(string(raw), `"pinned"`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the pin is invisible to `aura why`, which is where anyone would look")
	}
}

// A development affordance must not be usable where the record is the product.
func TestPinsAreRefusedInPublishedMode(t *testing.T) {
	_, _, _, err := pinnedSession(t, "published", map[string]Pin{"e": textPin("canned")})
	if err == nil {
		t.Fatal("published mode accepted pinned outputs")
	}
	if !strings.Contains(err.Error(), "published") {
		t.Fatalf("the refusal should say why, got %q", err)
	}
}

// A pin that could not have come out of that node is a lie downstream believes.
func TestPinMustNameARealEgressPort(t *testing.T) {
	_, _, _, err := pinnedSession(t, "local", map[string]Pin{
		"e": {Port: "not_a_port", Payload: json.RawMessage(`{}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "not_a_port") {
		t.Fatalf("want a refusal naming the port, got %v", err)
	}
}

func TestPinMustNameANodeInTheGraph(t *testing.T) {
	_, _, _, err := pinnedSession(t, "local", map[string]Pin{"ghost": textPin("x")})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want a refusal naming the node, got %v", err)
	}
}

func TestClientCannotBePinned(t *testing.T) {
	_, _, _, err := pinnedSession(t, "local", map[string]Pin{ClientRef: textPin("x")})
	if err == nil || !strings.Contains(err.Error(), "not a skill") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

// A declared schema that disagrees with the port's own is refused rather than
// quietly overwritten: the caller believes something false about their graph.
func TestPinWithTheWrongSchemaIsRefused(t *testing.T) {
	_, _, _, err := pinnedSession(t, "local", map[string]Pin{
		"e": {Port: "text_out", Schema: "std/audio-chunk@1", Payload: json.RawMessage(`{}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "std/text@1") {
		t.Fatalf("want a refusal naming the real schema, got %v", err)
	}
}

// The pinned payload is filled in from the manifest, so downstream sees the
// schema the port declares rather than whatever the caller left blank.
func TestPinnedEnvelopeCarriesThePortsDeclaredSchema(t *testing.T) {
	sess, _, toClient, err := pinnedSession(t, "local", map[string]Pin{"e": textPin("canned")})
	if err != nil {
		t.Fatalf("SetPins: %v", err)
	}
	sess.Route(clientData("p1"))
	for _, env := range *toClient {
		if env.Kind == channel.KindData && env.Schema != "std/text@1" {
			t.Fatalf("pinned data carried schema %q", env.Schema)
		}
	}
}

// No pins is the ordinary path and must cost nothing.
func TestSetPinsWithNothingIsANoOp(t *testing.T) {
	sess, delivered, _, err := pinnedSession(t, "local", nil)
	if err != nil {
		t.Fatalf("SetPins: %v", err)
	}
	sess.Route(clientData("p1"))
	if len(*delivered) != 1 {
		t.Fatalf("an unpinned session must still dispatch; got %d deliveries", len(*delivered))
	}
	if len(sess.Pinned()) != 0 {
		t.Fatal("Pinned() should be empty")
	}
}
