package executor

import (
	"encoding/json"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
)

const sampleTrace = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestValidTraceparent(t *testing.T) {
	good := []string{
		sampleTrace,
		"00-00000000000000000000000000000001-0000000000000001-00",
	}
	bad := []string{
		"",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",   // unknown version
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",   // zero trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",   // zero parent id
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",   // uppercase
		"00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01",    // short
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-x", // trailing
		"00_4bf92f3577b34da6a3ce929d0e0e4736_00f067aa0ba902b7_01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz",
	}
	for _, s := range good {
		if !channel.ValidTraceparent(s) {
			t.Errorf("%q was refused", s)
		}
	}
	for _, s := range bad {
		if channel.ValidTraceparent(s) {
			t.Errorf("%q was accepted", s)
		}
	}
}

func traceSession(t *testing.T) (*Session, *[]channel.Envelope, *[]channel.Envelope) {
	t.Helper()
	reg := registry.New()
	toSkill := liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	toClient := &[]channel.Envelope{}
	sess, err := NewSession("s", graphInto("s", "logical.echo", ""), reg, testStore(t), "local",
		DefaultPolicy(), nil, "", func(raw []byte, _ string) error {
			var env channel.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				return err
			}
			*toClient = append(*toClient, env)
			return nil
		}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return sess, toSkill, toClient
}

// The context a client supplies reaches the skill, and comes back on the
// answer even though the skill — like every skill built before the field
// existed — never echoed it.
func TestTraceContextRidesTheWholeChain(t *testing.T) {
	sess, toSkill, toClient := traceSession(t)

	ask := clientData("s")
	ask.Trace = sampleTrace
	sess.Route(ask)
	if len(*toSkill) != 1 || (*toSkill)[0].Trace != sampleTrace {
		t.Fatalf("the skill was handed trace %q", (*toSkill)[0].Trace)
	}

	// The skill answers with no trace field at all.
	reply := skillData("s", (*toSkill)[0].ID, 1, "hi")
	sess.Route(reply)
	if len(*toClient) != 1 || (*toClient)[0].Trace != sampleTrace {
		t.Fatalf("the answer came back with trace %q; the chain lost its context", (*toClient)[0].Trace)
	}
}

func TestNoTraceMeansNoFieldOnTheWire(t *testing.T) {
	sess, toSkill, _ := traceSession(t)
	sess.Route(clientData("s"))
	raw, _ := json.Marshal((*toSkill)[0])
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if _, present := wire["trace"]; present {
		t.Fatal("an envelope of a chain with no trace context carries a `trace` key; v1.6 peers would see a changed wire")
	}
}

func TestAMalformedTraceIsDroppedNotRefused(t *testing.T) {
	sess, toSkill, _ := traceSession(t)
	ask := clientData("s")
	ask.Trace = "not-a-traceparent"
	sess.Route(ask)
	if len(*toSkill) != 1 {
		t.Fatal("a delivery was refused because its tracing header was malformed")
	}
	if (*toSkill)[0].Trace != "" {
		t.Fatalf("a malformed trace %q was propagated", (*toSkill)[0].Trace)
	}
}

// Two chains in one session keep their own contexts.
func TestTraceContextIsPerChain(t *testing.T) {
	sess, toSkill, _ := traceSession(t)
	other := "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"

	first := clientData("s")
	first.Trace = sampleTrace
	second := clientData("s")
	second.ID, second.Idem, second.Seq, second.Trace = channel.NewID(), "s:2", 2, other
	third := clientData("s")
	third.ID, third.Idem, third.Seq = channel.NewID(), "s:3", 3

	sess.Route(first)
	sess.Route(second)
	sess.Route(third)
	got := []string{(*toSkill)[0].Trace, (*toSkill)[1].Trace, (*toSkill)[2].Trace}
	want := []string{sampleTrace, other, ""}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain %d carries %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTraceIndexIsBounded(t *testing.T) {
	idx := newTraceIndex(3)
	for _, root := range []string{"a", "b", "c", "d"} {
		idx.set(root, sampleTrace)
	}
	if idx.get("a") != "" {
		t.Fatal("the oldest chain was not evicted")
	}
	if idx.get("d") != sampleTrace || len(idx.byID) != 3 {
		t.Fatalf("index holds %d entries after 4 inserts at a bound of 3", len(idx.byID))
	}
}

// ── token metering ─────────────────────────────────────────────────────────

func attested(t *testing.T, usage string) json.RawMessage {
	t.Helper()
	att := `{"engine":"llama.cpp","model":"m"`
	if usage != "" {
		att += `,"usage":` + usage
	}
	return json.RawMessage(att + "}")
}

func TestReportedUsageIsMetered(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")
	sess, err := m.Start("s", "echo", "", func([]byte, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer m.End("s")

	ask := clientData("s")
	sess.Route(ask)
	delivered := deliveredTo(t, m, "s")

	reply := skillData("s", delivered, 1, "hi")
	reply.Attest = attested(t, `{"input_tokens":900,"output_tokens":40,"cached_input_tokens":768}`)
	sess.Route(reply)
	// The same envelope again: at-least-once delivery must not double the bill.
	sess.Route(reply)

	bare := skillData("s", delivered, 2, "again")
	bare.Attest = attested(t, "")
	sess.Route(bare)

	got := m.TokenUsage()
	want := TokenUsage{Input: 900, Output: 40, CachedInput: 768, Inferences: 1}
	if got != want {
		t.Fatalf("meter reads %+v, want %+v", got, want)
	}
}

func TestNonsenseUsageIsIgnored(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")
	sess, err := m.Start("s", "echo", "", func([]byte, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer m.End("s")
	sess.Route(clientData("s"))
	delivered := deliveredTo(t, m, "s")

	for i, usage := range []string{
		`{"input_tokens":-5,"output_tokens":-1}`,
		`{"input_tokens":"many"}`,
		`"lots"`,
		`{}`,
	} {
		reply := skillData("s", delivered, uint64(i+1), "x")
		reply.Attest = attested(t, usage)
		sess.Route(reply)
	}
	if got := m.TokenUsage(); got != (TokenUsage{}) {
		t.Fatalf("malformed usage moved the meter to %+v", got)
	}
}
