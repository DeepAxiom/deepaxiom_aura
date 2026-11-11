package executor

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"aura/kernel/internal/channel"
)

// collector is a watcher that takes everything.
type collector struct {
	mu     sync.Mutex
	frames []channel.Envelope
	closed []string
}

func (c *collector) offer(raw []byte) bool {
	var env channel.Envelope
	_ = json.Unmarshal(raw, &env)
	c.mu.Lock()
	c.frames = append(c.frames, env)
	c.mu.Unlock()
	return true
}

func (c *collector) onClose(reason string) {
	c.mu.Lock()
	c.closed = append(c.closed, reason)
	c.mu.Unlock()
}

func (c *collector) texts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, env := range c.frames {
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(env.Payload, &p)
		out = append(out, p.Text)
	}
	return out
}

func watchedSession(t *testing.T) (*Manager, *Session, *clientSink) {
	t.Helper()
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/logical/echo", "logical", "logical.echo")
	seedGraph(t, st, "echo", "logical.echo")
	client := &clientSink{}
	sess, err := m.Start("s", "echo", "", client.send)
	if err != nil {
		t.Fatal(err)
	}
	return m, sess, client
}

func TestWatchersSeeWhatTheClientSeesInOrder(t *testing.T) {
	m, sess, client := watchedSession(t)
	defer m.End("s")
	a, b := &collector{}, &collector{}
	for _, w := range []*collector{a, b} {
		if _, err := m.Watch("s", w.offer, w.onClose); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 50; i++ {
		sess.Route(skillData("s", "", uint64(i), fmt.Sprint(i)))
	}
	want := client.got()
	if len(want) != 50 {
		t.Fatalf("the client received %d frames", len(want))
	}
	for name, w := range map[string]*collector{"a": a, "b": b} {
		got := w.texts()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("watcher %s saw %v, the client saw %v", name, got, want)
		}
	}
	if sess.Watchers() != 2 || m.WatchStats().Watchers != 2 {
		t.Fatalf("watcher count reads %d / %d", sess.Watchers(), m.WatchStats().Watchers)
	}
}

// The property the feature stands on: an observer that cannot keep up is cut
// off, and neither the session nor the other observers notice.
func TestAWatcherThatFallsBehindIsCutOffAndNothingElseIsAffected(t *testing.T) {
	m, sess, client := watchedSession(t)
	defer m.End("s")
	good := &collector{}
	var slowClosed []string
	room := 3
	slow := func([]byte) bool {
		if room == 0 {
			return false
		}
		room--
		return true
	}
	if _, err := m.Watch("s", slow, func(r string) { slowClosed = append(slowClosed, r) }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Watch("s", good.offer, good.onClose); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		sess.Route(skillData("s", "", uint64(i), fmt.Sprint(i)))
	}
	if client.count.Load() != 20 {
		t.Fatalf("the client received %d of 20 frames while a watcher was behind", client.count.Load())
	}
	if got := good.texts(); len(got) != 20 {
		t.Fatalf("the healthy watcher received %d of 20 frames", len(got))
	}
	if len(slowClosed) != 1 {
		t.Fatalf("the slow watcher was told it was cut off %d times; want exactly once", len(slowClosed))
	}
	if len(good.closed) != 0 {
		t.Fatal("a healthy watcher was closed along with the slow one")
	}
	if sess.Watchers() != 1 {
		t.Fatalf("%d watchers remain; the one that fell behind should be gone", sess.Watchers())
	}
	if m.WatchStats().CutOff != 1 {
		t.Fatalf("cut_off reads %d", m.WatchStats().CutOff)
	}
}

// Who is asked to approve an effect is the client and nobody else.
func TestAGateIsNeverShownToTheAudience(t *testing.T) {
	m, reg, st := testManager(t)
	liveSkill(t, reg, "acme/motor/erp", "motor", "motor.erp.write")
	seedGraph(t, st, "write", "motor.erp.write")
	var toClient []channel.Envelope
	sess, err := m.Start("s", "write", "", func(raw []byte, _ string) error {
		var env channel.Envelope
		_ = json.Unmarshal(raw, &env)
		toClient = append(toClient, env)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.End("s")
	audience := &collector{}
	if _, err := m.Watch("s", audience.offer, audience.onClose); err != nil {
		t.Fatal(err)
	}

	sess.Route(clientData("s")) // into a motor skill: held at the gate

	asked := false
	for _, env := range toClient {
		if env.Kind == channel.KindConfirmRequest {
			asked = true
		}
	}
	if !asked {
		t.Fatal("the client was never asked to approve; the test's premise is wrong")
	}
	audience.mu.Lock()
	defer audience.mu.Unlock()
	for _, env := range audience.frames {
		if env.Kind == channel.KindConfirmRequest {
			t.Fatal("a confirm_request reached a watcher")
		}
	}
}

func TestEndingASessionClosesItsAudience(t *testing.T) {
	m, sess, _ := watchedSession(t)
	w := &collector{}
	if _, err := m.Watch("s", w.offer, w.onClose); err != nil {
		t.Fatal(err)
	}
	m.End("s")
	if len(w.closed) != 1 {
		t.Fatalf("the watcher was told the session ended %d times", len(w.closed))
	}
	if _, err := sess.Watch(w.offer, w.onClose); err == nil {
		t.Fatal("a watcher attached to a session that has ended")
	}
	if _, err := m.Watch("s", w.offer, w.onClose); err == nil {
		t.Fatal("a watcher attached by id to a session that is gone")
	}
}

func TestDetachingStopsDelivery(t *testing.T) {
	m, sess, _ := watchedSession(t)
	defer m.End("s")
	w := &collector{}
	cancel, err := m.Watch("s", w.offer, w.onClose)
	if err != nil {
		t.Fatal(err)
	}
	sess.Route(skillData("s", "", 1, "one"))
	cancel()
	cancel() // idempotent
	sess.Route(skillData("s", "", 2, "two"))
	if got := w.texts(); len(got) != 1 || got[0] != "one" {
		t.Fatalf("after detaching the watcher holds %v", got)
	}
	if len(w.closed) != 0 {
		t.Fatal("a watcher that detached itself was told it was closed")
	}
	if sess.Watchers() != 0 {
		t.Fatalf("%d watchers after the only one detached", sess.Watchers())
	}
}

func TestTheAudienceIsBounded(t *testing.T) {
	m, sess, _ := watchedSession(t)
	defer m.End("s")
	noop := func([]byte) bool { return true }
	for i := 0; i < MaxWatchersPerSession; i++ {
		if _, err := sess.Watch(noop, nil); err != nil {
			t.Fatalf("watcher %d refused inside the bound: %v", i, err)
		}
	}
	if _, err := sess.Watch(noop, nil); err == nil {
		t.Fatal("a session accepted more watchers than its bound")
	}
}

// Watching causes nothing, so it leaves nothing in the causal log.
func TestWatchersAreNotInTheCausalLog(t *testing.T) {
	m, sess, _ := watchedSession(t)
	sess.Route(skillData("s", "", 1, "one"))
	before, _, _ := m.st.SessionEvents("s", 100)
	w := &collector{}
	cancel, _ := m.Watch("s", w.offer, w.onClose)
	cancel()
	after, _, _ := m.st.SessionEvents("s", 100)
	if len(after) != len(before) {
		t.Fatalf("attaching and detaching a watcher wrote %d events", len(after)-len(before))
	}
	m.End("s")
}

func TestConcurrentWatchersAttachAndDetachUnderTraffic(t *testing.T) {
	m, sess, client := watchedSession(t)
	defer m.End("s")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				w := &collector{}
				cancel, err := m.Watch("s", w.offer, w.onClose)
				if err == nil {
					cancel()
				}
			}
		}()
	}
	for i := 1; i <= 2000; i++ {
		sess.Route(skillData("s", "", uint64(i), fmt.Sprint(i)))
	}
	close(stop)
	wg.Wait()
	if client.count.Load() != 2000 {
		t.Fatalf("the client received %d of 2000 frames under watcher churn", client.count.Load())
	}
}
