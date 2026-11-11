package executor

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/registry"
)

// skillData is an envelope a skill emits into a session, as the gateway's
// read loop would hand it to Dispatch.
func skillData(session, cause string, seq uint64, text string) channel.Envelope {
	payload, _ := json.Marshal(map[string]any{"text": text, "final": false})
	return channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: cause, Session: session,
		Node: "s", Port: "text_out", Seq: seq,
		Idem:   fmt.Sprintf("%s:s:text_out:%d", session, seq),
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	}
}

// clientSink collects what a session delivers to its client, optionally
// blocking until released — a consumer that has fallen behind.
type clientSink struct {
	mu    sync.Mutex
	texts []string
	gate  chan struct{} // nil = never blocks
	count atomic.Int64
}

func (c *clientSink) send(raw []byte, _ string) error {
	if c.gate != nil {
		<-c.gate
	}
	var env channel.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(env.Payload, &p)
	c.mu.Lock()
	c.texts = append(c.texts, p.Text)
	c.mu.Unlock()
	c.count.Add(1)
	return nil
}

func (c *clientSink) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.texts...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mailboxManager(t *testing.T, depth int) *Manager {
	t.Helper()
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")
	m.SetMailboxDepth(depth)
	return m
}

// The reason mailboxes exist. Two sessions share one producer; the consumer
// of the first stops reading. Inline, the producer's read loop would be stuck
// inside the first session's delivery and the second session would get
// nothing. With mailboxes the second is unaffected.
func TestASlowSessionDoesNotStallAnother(t *testing.T) {
	m := mailboxManager(t, 64)
	slow := &clientSink{gate: make(chan struct{})}
	fast := &clientSink{}
	if _, err := m.Start("slow", "echo", "", slow.send); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("fast", "echo", "", fast.send); err != nil {
		t.Fatal(err)
	}
	defer m.End("fast")
	defer func() { close(slow.gate); m.End("slow") }()

	// One read loop, as the gateway has: slow's envelope first, then fast's.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Dispatch(skillData("slow", "", 1, "stuck"))
		for i := 1; i <= 20; i++ {
			_ = m.Dispatch(skillData("fast", "", uint64(i), fmt.Sprint(i)))
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the read loop blocked behind a session whose consumer is not reading")
	}
	waitFor(t, "the fast session's 20 deliveries", func() bool { return fast.count.Load() == 20 })
	if slow.count.Load() != 0 {
		t.Fatalf("the slow session delivered %d envelopes while its consumer was blocked", slow.count.Load())
	}
}

// The same scenario inline, to show the test above is measuring the mailbox
// and not something that was already true.
func TestInlineDispatchDoesStallAnother(t *testing.T) {
	m := mailboxManager(t, 0)
	slow := &clientSink{gate: make(chan struct{})}
	fast := &clientSink{}
	if _, err := m.Start("slow", "echo", "", slow.send); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("fast", "echo", "", fast.send); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Dispatch(skillData("slow", "", 1, "stuck"))
		_ = m.Dispatch(skillData("fast", "", 1, "1"))
	}()
	select {
	case <-done:
		t.Fatal("inline dispatch returned while the consumer was blocked; the premise of the mailbox is wrong")
	case <-time.After(150 * time.Millisecond):
	}
	if fast.count.Load() != 0 {
		t.Fatal("the fast session was served past a blocked inline dispatch")
	}
	close(slow.gate)
	<-done
	m.End("slow")
	m.End("fast")
}

func TestMailboxPreservesOrderWithinASession(t *testing.T) {
	m := mailboxManager(t, 8) // small, so the producer blocks and resumes many times
	sink := &clientSink{}
	if _, err := m.Start("s", "echo", "", sink.send); err != nil {
		t.Fatal(err)
	}
	defer m.End("s")

	const n = 2000
	for i := 1; i <= n; i++ {
		if err := m.Dispatch(skillData("s", "", uint64(i), fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every delivery", func() bool { return sink.count.Load() == n })
	for i, text := range sink.got() {
		if text != fmt.Sprint(i+1) {
			t.Fatalf("delivery %d carries %q; a mailbox reordered a session's stream", i, text)
		}
	}
}

func TestMailboxKeepsEverySessionSeparateUnderLoad(t *testing.T) {
	m := mailboxManager(t, 32)
	const sessions, perSession = 40, 200
	sinks := make([]*clientSink, sessions)
	for i := range sinks {
		sinks[i] = &clientSink{}
		if _, err := m.Start(fmt.Sprintf("s%d", i), "echo", "", sinks[i].send); err != nil {
			t.Fatal(err)
		}
	}
	// Four producer connections, each interleaving all sessions.
	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for j := p; j < perSession; j += 4 {
				for i := 0; i < sessions; i++ {
					id := fmt.Sprintf("s%d", i)
					_ = m.Dispatch(skillData(id, "", uint64(j+1), fmt.Sprintf("%d/%d", i, j)))
				}
			}
		}(p)
	}
	wg.Wait()
	for i, sink := range sinks {
		i, sink := i, sink
		waitFor(t, fmt.Sprintf("session %d", i), func() bool { return sink.count.Load() == perSession })
		for _, text := range sink.got() {
			var session, n int
			if _, err := fmt.Sscanf(text, "%d/%d", &session, &n); err != nil || session != i {
				t.Fatalf("session %d received %q, which belongs to another session", i, text)
			}
		}
	}
	for i := range sinks {
		m.End(fmt.Sprintf("s%d", i))
	}
	if q := m.MailboxStats().Queued; q != 0 {
		t.Fatalf("%d envelopes still counted as queued after every session ended", q)
	}
}

// Every envelope a mailbox drains must still reach the causal log: the queue
// sits in front of the log, it does not replace it.
func TestMailboxedEnvelopesAreStillLogged(t *testing.T) {
	m := mailboxManager(t, 16)
	sink := &clientSink{}
	if _, err := m.Start("s", "echo", "", sink.send); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 50; i++ {
		_ = m.Dispatch(skillData("s", "", uint64(i), fmt.Sprint(i)))
	}
	waitFor(t, "every delivery", func() bool { return sink.count.Load() == 50 })
	m.End("s")

	events, _, err := m.st.SessionEvents("s", 1000)
	if err != nil {
		t.Fatal(err)
	}
	// One record for each envelope the skill emitted and one for each the
	// kernel forwarded to the client.
	if len(events) != 100 {
		t.Fatalf("the causal log holds %d events for 50 routed envelopes; want 100", len(events))
	}
}

// A client's cancel does not pass through the mailbox, so it overtakes output
// still queued behind a slow consumer — and that output is then suppressed
// instead of delivered.
func TestCancelOvertakesQueuedOutput(t *testing.T) {
	m := mailboxManager(t, 256)
	sink := &clientSink{gate: make(chan struct{})}
	sess, err := m.Start("s", "echo", "", sink.send)
	if err != nil {
		t.Fatal(err)
	}
	defer m.End("s")

	ask := clientData("s")
	sess.Route(ask)
	// The skill received the question as a kernel-forwarded envelope; what it
	// emits names that delivery as its cause.
	delivered := deliveredTo(t, m, "s")

	for i := 1; i <= 100; i++ {
		_ = m.Dispatch(skillData("s", delivered, uint64(i), fmt.Sprint(i)))
	}
	// The drain is now stuck on the first delivery; 99 wait in the mailbox.
	sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: ask.ID,
		Session: "s", Kind: channel.KindCancel,
	})
	close(sink.gate)

	waitFor(t, "the mailbox to drain", func() bool { return m.MailboxStats().Queued == 0 })
	time.Sleep(20 * time.Millisecond)
	if got := sink.count.Load(); got > 2 {
		t.Fatalf("%d envelopes reached the client after a cancel that arrived while they were queued", got)
	}
}

// deliveredTo returns the id of the envelope the kernel forwarded into the
// skill for a session, read back from the causal log.
func deliveredTo(t *testing.T, m *Manager, session string) string {
	t.Helper()
	events, _, err := m.st.SessionEvents(session, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range events {
		var env channel.Envelope
		if json.Unmarshal(raw, &env) == nil && env.Node == "s" && env.Port == "text_in" {
			return env.ID
		}
	}
	t.Fatal("the kernel never forwarded the client's message to the skill")
	return ""
}

// Ending a session with work still queued must neither hang nor leak the
// drain goroutine, and a producer blocked on that full mailbox must be let go.
func TestEndingASessionReleasesABlockedProducer(t *testing.T) {
	m := mailboxManager(t, 2)
	sink := &clientSink{gate: make(chan struct{})}
	if _, err := m.Start("s", "echo", "", sink.send); err != nil {
		t.Fatal(err)
	}
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for i := 1; i <= 50; i++ {
			_ = m.Dispatch(skillData("s", "", uint64(i), fmt.Sprint(i)))
		}
	}()
	waitFor(t, "the producer to hit the full mailbox", func() bool { return m.MailboxStats().Waits > 0 })

	ended := make(chan struct{})
	go func() { defer close(ended); m.End("s") }()
	// End waits for the drain goroutine, which is inside a delivery to the
	// blocked consumer; releasing the consumer is what lets it finish.
	close(sink.gate)
	for _, ch := range []chan struct{}{ended, produced} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("ending a session left a goroutine blocked")
		}
	}
	if err := m.Dispatch(skillData("s", "", 99, "late")); err == nil {
		t.Fatal("an envelope for an ended session was accepted")
	}
	if q := m.MailboxStats().Queued; q != 0 {
		t.Fatalf("queued gauge is %d after the session ended", q)
	}
}

func TestAPanicInOneSessionDoesNotKillItsMailbox(t *testing.T) {
	m := mailboxManager(t, 16)
	sink := &clientSink{}
	calls := 0
	send := func(raw []byte, qos string) error {
		calls++
		if calls == 1 {
			panic("a bug in a delivery path")
		}
		return sink.send(raw, qos)
	}
	if _, err := m.Start("s", "echo", "", send); err != nil {
		t.Fatal(err)
	}
	defer m.End("s")
	_ = m.Dispatch(skillData("s", "", 1, "boom"))
	_ = m.Dispatch(skillData("s", "", 2, "after"))
	waitFor(t, "the envelope after the panic", func() bool { return sink.count.Load() == 1 })
	if got := sink.got(); len(got) != 1 || got[0] != "after" {
		t.Fatalf("after a panic the session delivered %v", got)
	}
}

func TestInlineIsStillTheDefault(t *testing.T) {
	m, _, _ := testManager(t)
	if m.MailboxStats().Enabled {
		t.Fatal("a Manager nobody configured dispatches through mailboxes; embedders rely on inline")
	}
}

// ── replica load ───────────────────────────────────────────────────────────

func replica(t *testing.T, reg *registry.Registry, conn string) *registry.Live {
	t.Helper()
	m := registry.Manifest{
		ID: "acme/logical/echo", Version: "1.0.0", Protocol: "1", Name: "echo",
		Description: "test skill", Capability: "logical.echo", Type: "logical", Format: "source",
	}
	m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
	m.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}
	l := &registry.Live{Manifest: m, Connected: time.Unix(1700000000, 0),
		Send: func([]byte, string) error { return nil }}
	reg.Register(conn, l)
	return l
}

// Long sessions pile up on whichever replica they landed on; the next session
// must go to the one with room rather than to the next in rotation.
func TestNewSessionsGoToTheQuietestReplica(t *testing.T) {
	m, reg, st := testManager(t)
	seedGraph(t, st, "echo", "logical.echo")
	a := replica(t, reg, "conn-a")
	b := replica(t, reg, "conn-b")
	noop := func([]byte, string) error { return nil }

	for i := 0; i < 6; i++ {
		if _, err := m.Start(fmt.Sprintf("long%d", i), "echo", "", noop); err != nil {
			t.Fatal(err)
		}
	}
	if a.Load() != 3 || b.Load() != 3 {
		t.Fatalf("six sessions landed %d/%d across two replicas; want 3/3", a.Load(), b.Load())
	}
	// Everything on one replica ends; the other keeps its three.
	ended := 0
	for i := 0; i < 6 && ended < 3; i++ {
		id := fmt.Sprintf("long%d", i)
		if sess, _ := m.Get(id); sess != nil && sess.replicas[0] == a {
			m.End(id)
			ended++
		}
	}
	if a.Load() != 0 || b.Load() != 3 {
		t.Fatalf("after ending a's sessions the loads are %d/%d; want 0/3", a.Load(), b.Load())
	}
	for i := 0; i < 3; i++ {
		if _, err := m.Start(fmt.Sprintf("new%d", i), "echo", "", noop); err != nil {
			t.Fatal(err)
		}
	}
	if a.Load() != 3 || b.Load() != 3 {
		t.Fatalf("three new sessions left the loads at %d/%d; they should all have gone to the empty replica",
			a.Load(), b.Load())
	}
}

func TestEndingASessionTwiceReleasesItsLoadOnce(t *testing.T) {
	m, reg, st := testManager(t)
	seedGraph(t, st, "echo", "logical.echo")
	a := replica(t, reg, "conn-a")
	noop := func([]byte, string) error { return nil }
	if _, err := m.Start("s1", "echo", "", noop); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("s2", "echo", "", noop); err != nil {
		t.Fatal(err)
	}
	m.End("s1")
	m.End("s1") // the socket closing and the reaper can both get here
	if a.Load() != 1 {
		t.Fatalf("load is %d after ending one of two sessions twice; want 1", a.Load())
	}
}

// A session that fails to start must not leave load behind.
func TestAFailedStartHoldsNoLoad(t *testing.T) {
	m, reg, st := testManager(t)
	a := replica(t, reg, "conn-a")
	// A graph wired to a port the skill does not have: resolution succeeds,
	// wiring fails.
	g := map[string]any{
		"ir": IRMajor, "graph_id": "bad", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "s", "resolve": "logical.echo"}},
		"edges": []map[string]any{{"from": "client.text_out", "to": "s.no_such_port"}},
	}
	raw, _ := json.Marshal(g)
	if err := st.SaveGraph("bad", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("s1", "bad", "", func([]byte, string) error { return nil }); err == nil {
		t.Fatal("a graph wired to a missing port started")
	}
	if a.Load() != 0 {
		t.Fatalf("a session that never started left load %d on its replica", a.Load())
	}
}
