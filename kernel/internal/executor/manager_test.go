package executor

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
)

// Manager owns every live session on a node. It had no tests, which included
// the session cap — the thing standing between a public ingress route and an
// out-of-memory condition.

func seedGraph(t *testing.T, st *store.Store, id, capability string) {
	t.Helper()
	g := map[string]any{
		"ir": IRMajor, "graph_id": id, "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "s", "resolve": capability}},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "s.text_in"},
			{"from": "s.text_out", "to": "client.text_in"},
		},
	}
	raw, _ := json.Marshal(g)
	if err := st.SaveGraph(id, raw); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}
}

func testManager(t *testing.T) (*Manager, *registry.Registry, *store.Store) {
	t.Helper()
	reg := registry.New()
	st := testStore(t)
	return NewManager(reg, st, "local", DefaultPolicy(), nil, testLogger()), reg, st
}

func TestManagerStartsAndEndsSessions(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")

	if m.LiveSessions() != 0 {
		t.Fatalf("a fresh manager reports %d sessions", m.LiveSessions())
	}
	if _, err := m.Start("s1", "echo", func([]byte, string) error { return nil }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.LiveSessions() != 1 {
		t.Errorf("after one Start: %d live sessions", m.LiveSessions())
	}
	m.End("s1")
	if m.LiveSessions() != 0 {
		t.Errorf("after End: %d live sessions; the session leaked", m.LiveSessions())
	}
}

func TestStartingAnUnknownGraphFails(t *testing.T) {
	m, _, _ := testManager(t)
	if _, err := m.Start("s1", "no-such-graph", func([]byte, string) error { return nil }); err == nil {
		t.Fatal("starting a graph that was never registered succeeded")
	}
}

// A graph whose skill is not connected cannot run, and the failure belongs at
// Start rather than on the first message.
func TestStartingAGraphWithNoLiveSkillFails(t *testing.T) {
	m, _, st := testManager(t)
	seedGraph(t, st, "echo", "logical.nobody-provides-this")

	if _, err := m.Start("s1", "echo", func([]byte, string) error { return nil }); err == nil {
		t.Fatal("a graph resolving to nothing started anyway")
	}
}

// The cap is what stops /hooks/{name} — an endpoint whose call rate the node
// does not control — turning into a session generator.
func TestSessionCapRefusesBeyondItsLimit(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")
	m.SetMaxSessions(3)

	for i := 0; i < 3; i++ {
		if _, err := m.Start(fmt.Sprintf("s%d", i), "echo",
			func([]byte, string) error { return nil }); err != nil {
			t.Fatalf("session %d refused inside the cap: %v", i, err)
		}
	}
	_, err := m.Start("s-overflow", "echo", func([]byte, string) error { return nil })
	if err == nil {
		t.Fatal("the manager opened a fourth session against a cap of 3")
	}
	// The operator has to be able to tell this apart from a broken graph.
	if got := err.Error(); !contains(got, "session limit") {
		t.Errorf("refusal reads %q; it should name the limit", got)
	}

	// Ending one makes room again, or a node would wedge after its first burst.
	m.End("s0")
	if _, err := m.Start("s-again", "echo", func([]byte, string) error { return nil }); err != nil {
		t.Fatalf("no room freed after ending a session: %v", err)
	}
}

func TestZeroCapMeansUnlimited(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")
	m.SetMaxSessions(0)

	for i := 0; i < 50; i++ {
		if _, err := m.Start(fmt.Sprintf("s%d", i), "echo",
			func([]byte, string) error { return nil }); err != nil {
			t.Fatalf("session %d refused with no cap set: %v", i, err)
		}
	}
}

func TestDispatchRoutesToTheOwningSession(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")

	var toClient []channel.Envelope
	var mu sync.Mutex
	if _, err := m.Start("s1", "echo", func(raw []byte, _ string) error {
		var env channel.Envelope
		_ = json.Unmarshal(raw, &env)
		mu.Lock()
		toClient = append(toClient, env)
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	payload, _ := json.Marshal(map[string]any{"text": "hi", "final": true})
	err := m.Dispatch(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: "s1",
		Node: "s", Port: "text_out", Seq: 1, Idem: "s1:s:text_out:1",
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(toClient) != 1 {
		t.Fatalf("the client received %d envelopes; want 1", len(toClient))
	}
}

// An envelope for a session this node does not own is an error rather than a
// silent drop: it means a skill outlived its session, which is worth seeing.
func TestDispatchToAnUnknownSessionIsAnError(t *testing.T) {
	m, _, _ := testManager(t)
	err := m.Dispatch(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: "ghost",
		Kind: channel.KindData,
	})
	if err == nil {
		t.Fatal("dispatching into a session that does not exist reported success")
	}
}

func TestManagerExposesThePolicyInForce(t *testing.T) {
	pol := mustLoadPolicy(t, "policy: 1\ndefault_effect: deny\n")
	m := NewManager(registry.New(), testStore(t), "local", pol, nil, testLogger())

	if m.Policy().Hash() != pol.Hash() {
		t.Error("the manager reports a different policy than it was given")
	}
	// A nil policy has to become the permissive default rather than a nil
	// dereference on the first session.
	fallback := NewManager(registry.New(), testStore(t), "local", nil, nil, testLogger())
	if fallback.Policy() == nil {
		t.Fatal("a manager built with no policy has none at all")
	}
	if !fallback.Policy().GraphWaiverAllowed() {
		t.Error("the fallback policy is not the permissive default")
	}
}

// Concurrent starts and ends are the normal case on a busy node, and the
// manager is the shared structure they all touch.
func TestConcurrentStartAndEndIsSafe(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("s%d", n)
			if _, err := m.Start(id, "echo", func([]byte, string) error { return nil }); err == nil {
				m.End(id)
			}
		}(i)
	}
	wg.Wait()

	if live := m.LiveSessions(); live != 0 {
		t.Errorf("%d sessions survived; every one was ended", live)
	}
}
