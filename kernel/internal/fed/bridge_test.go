package fed

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// fakeNode is a minimal stand-in for a kernel: it answers /v1/skills, accepts
// a relay graph, and hands each /v1/stream socket to a script. It exists
// because the properties under test here — that a cancel crosses the bridge,
// that idem is derived rather than minted — are properties of what the bridge
// puts on the wire, and the cheapest honest way to see the wire is to be the
// other end of it.
type fakeNode struct {
	srv *httptest.Server

	mu           sync.Mutex
	graphPosts   int
	lastGraph    map[string]any
	streamOpened int
	received     []channel.Envelope
	authSeen     []string

	// onStream drives one client session; nil means "reply once and finish".
	onStream func(t *testing.T, conn *websocket.Conn, first channel.Envelope)
}

func newFakeNode(t *testing.T, skills []map[string]any) *fakeNode {
	t.Helper()
	n := &fakeNode{}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		n.recordAuth(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(skills)
	})
	mux.HandleFunc("/v1/graphs", func(w http.ResponseWriter, r *http.Request) {
		n.recordAuth(r)
		var g map[string]any
		_ = json.NewDecoder(r.Body).Decode(&g)
		n.mu.Lock()
		n.graphPosts++
		n.lastGraph = g
		n.mu.Unlock()
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"graph_id":"ok"}`))
	})
	mux.HandleFunc("/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		n.recordAuth(r)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		n.mu.Lock()
		n.streamOpened++
		n.mu.Unlock()

		hello := channel.Envelope{V: "1", ID: channel.NewID(), Kind: channel.KindStatus}
		if conn.WriteJSON(hello) != nil {
			return
		}
		var first channel.Envelope
		if conn.ReadJSON(&first) != nil {
			return
		}
		n.mu.Lock()
		n.received = append(n.received, first)
		n.mu.Unlock()

		if n.onStream != nil {
			n.onStream(t, conn, first)
			return
		}
		_ = conn.WriteJSON(channel.Envelope{
			V: "1", ID: channel.NewID(), Kind: channel.KindData,
			Port: "text_out", Schema: "std/text@1",
			Payload: json.RawMessage(`{"text":"remote reply","final":true}`),
		})
		_ = conn.WriteJSON(channel.Envelope{V: "1", ID: channel.NewID(), Kind: channel.KindDone})
	})

	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNode) recordAuth(r *http.Request) {
	n.mu.Lock()
	n.authSeen = append(n.authSeen, r.Header.Get("Authorization"))
	n.mu.Unlock()
}

func (n *fakeNode) httpURL() string { return n.srv.URL }
func (n *fakeNode) wsURL() string   { return "ws" + strings.TrimPrefix(n.srv.URL, "http") }

func (n *fakeNode) snapshot() (posts, streams int, got []channel.Envelope) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.graphPosts, n.streamOpened, append([]channel.Envelope(nil), n.received...)
}

// localStub stands in for the local node's /ws/skill: it accepts the proxy's
// registration and lets a test push envelopes at the bridge and read back
// whatever it emits.
type localStub struct {
	srv *httptest.Server

	mu       sync.Mutex
	manifest map[string]any
	emitted  []channel.Envelope

	toBridge chan channel.Envelope
	ready    chan struct{}
	once     sync.Once
}

func newLocalStub(t *testing.T) *localStub {
	t.Helper()
	s := &localStub{
		toBridge: make(chan channel.Envelope, 8),
		ready:    make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/skill", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var reg channel.Envelope
		if conn.ReadJSON(&reg) != nil {
			return
		}
		var m map[string]any
		_ = json.Unmarshal(reg.Payload, &m)
		s.mu.Lock()
		s.manifest = m
		s.mu.Unlock()

		_ = conn.WriteJSON(channel.Envelope{
			V: "1", ID: channel.NewID(), CauseID: reg.ID, Kind: channel.KindStatus,
			Payload: json.RawMessage(`{"state":"registered"}`),
		})
		s.once.Do(func() { close(s.ready) })

		go func() {
			for env := range s.toBridge {
				if conn.WriteJSON(env) != nil {
					return
				}
			}
		}()
		for {
			var env channel.Envelope
			if conn.ReadJSON(&env) != nil {
				return
			}
			s.mu.Lock()
			s.emitted = append(s.emitted, env)
			s.mu.Unlock()
		}
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *localStub) wsSkill() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/ws/skill"
}

func (s *localStub) emissions() []channel.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channel.Envelope(nil), s.emitted...)
}

func (s *localStub) proxyManifest() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.manifest
}

func remoteCatalog() []map[string]any {
	return []map[string]any{{
		"id": "acme/motor/writer", "name": "Writer", "description": "writes",
		"capability": "motor.api.writer", "type": "motor",
		"ports": map[string]any{
			"ingress": []map[string]string{{"name": "text_in", "schema": "std/text@1"}},
			"egress": []map[string]string{
				{"name": "text_out", "schema": "std/text@1"},
				{"name": "status_out", "schema": "std/status@1"},
			},
		},
	}}
}

func startBridge(t *testing.T, local *localStub, remote *fakeNode) context.CancelFunc {
	t.Helper()
	b := &Bridge{
		LocalWS:    local.wsSkill(),
		LocalHTTP:  local.srv.URL,
		RemoteHTTP: remote.httpURL(),
		RemoteWS:   remote.wsURL(),
		Log:        testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = b.Run(ctx) }()
	select {
	case <-local.ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the proxy never registered with the local node")
	}
	t.Cleanup(cancel)
	return cancel
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func dataEnvelope(id, idem string) channel.Envelope {
	return channel.Envelope{
		V: "1", ID: id, Session: "sess-1", Node: "fedskill", Port: "text_in",
		Seq: 1, Idem: idem, Schema: "std/text@1", Kind: channel.KindData,
		Payload: json.RawMessage(`{"text":"do it","final":true}`),
	}
}

// --- the guarantees that used to stop at the node boundary -------------------

// C3 requires a cancel to reach every skill working on a chain, at any depth.
// The bridge used to drop every envelope that was not `data`, so a cancel died
// at the boundary: the local kernel suppressed its own side while the remote
// node kept working. Barge-in across a federation did not happen.
func TestCancelCrossesTheBridge(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())

	remoteClosed := make(chan struct{})
	var closeOnce sync.Once
	remote.onStream = func(t *testing.T, conn *websocket.Conn, _ channel.Envelope) {
		// Hold the session open and stream until the far end goes away. A
		// propagated cancel closes this socket; without one we sit here until
		// the test's deadline.
		for {
			if conn.WriteJSON(channel.Envelope{
				V: "1", ID: channel.NewID(), Kind: channel.KindData,
				Port: "text_out", Schema: "std/text@1",
				Payload: json.RawMessage(`{"text":"tok","final":false}`),
			}) != nil {
				closeOnce.Do(func() { close(remoteClosed) })
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	local := newLocalStub(t)
	startBridge(t, local, remote)

	in := dataEnvelope("ENV-1", "sess-1:client:text_out:1")
	local.toBridge <- in
	waitFor(t, func() bool { _, streams, _ := remote.snapshot(); return streams > 0 },
		"the bridge never opened a remote session")

	// The kernel addresses a cancel with the cause_id the proxy knows the work
	// by, which is the id of the envelope it was handed.
	local.toBridge <- channel.Envelope{
		V: "1", ID: channel.NewID(), CauseID: in.ID,
		Session: "sess-1", Kind: channel.KindCancel,
	}

	select {
	case <-remoteClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancel did not reach the remote node; the relay kept streaming")
	}
}

// C3 rule 2 makes idem the deduplication key for the whole downstream chain.
// A fresh one per relayed envelope meant a retry looked like new work to every
// skill past the bridge — at-least-once delivery became at-least-once
// execution as soon as a federation was involved.
func TestIdemIsDerivedNotMinted(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	in := dataEnvelope("ENV-1", "sess-1:client:text_out:1")
	local.toBridge <- in
	waitFor(t, func() bool { _, _, got := remote.snapshot(); return len(got) > 0 },
		"nothing reached the remote node")

	_, _, got := remote.snapshot()
	if !strings.HasPrefix(got[0].Idem, in.Idem) {
		t.Fatalf("relayed idem = %q; it must derive from the incoming %q, "+
			"or a retry is indistinguishable from new work", got[0].Idem, in.Idem)
	}
}

// The same incoming envelope relayed twice must present the same idem, which
// is the property that actually makes a retry deduplicable.
func TestRetryOfTheSameWorkProducesTheSameIdem(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	in := dataEnvelope("ENV-1", "sess-1:client:text_out:1")
	local.toBridge <- in
	waitFor(t, func() bool { _, _, got := remote.snapshot(); return len(got) >= 1 }, "first relay")

	retry := dataEnvelope("ENV-2", "sess-1:client:text_out:1") // same idem, new id
	local.toBridge <- retry
	waitFor(t, func() bool { _, _, got := remote.snapshot(); return len(got) >= 2 }, "second relay")

	_, _, got := remote.snapshot()
	if got[0].Idem != got[1].Idem {
		t.Fatalf("the same work relayed twice produced %q then %q", got[0].Idem, got[1].Idem)
	}
}

// --- efficiency and fidelity --------------------------------------------------

// The relay graph is deterministic and keyed by capability, so posting it on
// every envelope was pure overhead on the hot path — and its error was being
// discarded, so a rejection surfaced later as an unexplained failure.
func TestRemoteGraphIsRegisteredOncePerSession(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	for i := 0; i < 5; i++ {
		local.toBridge <- dataEnvelope(channel.NewID(), "sess-1:client:text_out:1")
	}
	waitFor(t, func() bool { _, _, got := remote.snapshot(); return len(got) >= 5 },
		"not all envelopes were relayed")

	posts, _, _ := remote.snapshot()
	if posts != 1 {
		t.Fatalf("the relay graph was posted %d times for 5 envelopes; want 1", posts)
	}
}

// A skill whose ports carry different schemas has to stay distinguishable
// across the bridge. Collapsing every egress onto client.text_in made a
// transcript, a status and an audio frame arrive indistinguishable.
func TestEveryRemoteEgressPortKeepsItsIdentity(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	local.toBridge <- dataEnvelope("ENV-1", "sess-1:client:text_out:1")
	waitFor(t, func() bool { posts, _, _ := remote.snapshot(); return posts > 0 },
		"the relay graph was never registered")

	remote.mu.Lock()
	graph := remote.lastGraph
	remote.mu.Unlock()

	edges, _ := graph["edges"].([]any)
	targets := map[string]bool{}
	for _, raw := range edges {
		e, _ := raw.(map[string]any)
		to, _ := e["to"].(string)
		if strings.HasPrefix(to, "client.") {
			targets[to] = true
		}
	}
	if len(targets) != 2 {
		t.Fatalf("the relay graph funnels the remote egress ports into %v; "+
			"each one needs its own client port", targets)
	}
	for _, want := range []string{"client.text_out", "client.status_out"} {
		if !targets[want] {
			t.Errorf("no edge to %s; that port's replies would be unidentifiable", want)
		}
	}
}

// A federated motor skill must still look like a motor skill locally, or a
// federation becomes a way to launder an effect past the node's policy.
func TestProxyManifestPreservesTheEffectType(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	waitFor(t, func() bool { return local.proxyManifest() != nil }, "no manifest registered")
	m := local.proxyManifest()

	if m["type"] != "motor" {
		t.Fatalf("proxy type = %v; a federated effect must stay an effect", m["type"])
	}
	if m["capability"] != "motor.api.writer" {
		t.Fatalf("proxy capability = %v; policy resolves on this", m["capability"])
	}
}

// --- authentication ----------------------------------------------------------

// A bridge holds no privilege of its own: it authenticates to both nodes like
// any other client.
func TestBridgePresentsItsTokensToBothEnds(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)

	b := &Bridge{
		LocalWS: local.wsSkill(), LocalHTTP: local.srv.URL,
		RemoteHTTP: remote.httpURL(), RemoteWS: remote.wsURL(),
		LocalToken: "local-tok", RemoteToken: "remote-tok",
		Log: testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	waitFor(t, func() bool {
		remote.mu.Lock()
		defer remote.mu.Unlock()
		return len(remote.authSeen) > 0
	}, "the bridge never called the remote node")

	remote.mu.Lock()
	seen := append([]string(nil), remote.authSeen...)
	remote.mu.Unlock()
	for _, h := range seen {
		if h != "Bearer remote-tok" {
			t.Fatalf("remote call carried %q; want the remote token", h)
		}
	}
}

func TestRemoteRequiringAuthIsReportedClearly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/skills", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := &Bridge{
		RemoteHTTP: srv.URL,
		RemoteWS:   "ws" + strings.TrimPrefix(srv.URL, "http"),
		Log:        testLogger(),
	}
	err := b.Run(context.Background())
	if err == nil {
		t.Fatal("federating against a node that refused us reported success")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q; it should say the remote wants a token", err)
	}
}

// --- capability filtering -----------------------------------------------------

func TestCapabilityFilterSelectsWhatIsProxied(t *testing.T) {
	catalog := append(remoteCatalog(), map[string]any{
		"id": "acme/cognitive/chat", "name": "Chat", "description": "thinks",
		"capability": "cognitive.llm.chat", "type": "cognitive",
		"ports": map[string]any{
			"ingress": []map[string]string{{"name": "text_in", "schema": "std/text@1"}},
			"egress":  []map[string]string{{"name": "text_out", "schema": "std/text@1"}},
		},
	})
	remote := newFakeNode(t, catalog)
	local := newLocalStub(t)

	b := &Bridge{
		LocalWS: local.wsSkill(), LocalHTTP: local.srv.URL,
		RemoteHTTP: remote.httpURL(), RemoteWS: remote.wsURL(),
		CapFilter: "cognitive", Log: testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	waitFor(t, func() bool { return local.proxyManifest() != nil }, "nothing was proxied")
	if got := local.proxyManifest()["capability"]; got != "cognitive.llm.chat" {
		t.Fatalf("proxied %v; the filter should have selected the cognitive skill", got)
	}
}

func TestNoMatchingCapabilityIsAnError(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)

	b := &Bridge{
		LocalWS: local.wsSkill(), LocalHTTP: local.srv.URL,
		RemoteHTTP: remote.httpURL(), RemoteWS: remote.wsURL(),
		CapFilter: "sensorial", Log: testLogger(),
	}
	if err := b.Run(context.Background()); err == nil {
		t.Fatal("federating a capability the remote does not have reported success")
	}
}

// --- replies ------------------------------------------------------------------

func TestRepliesFlowBackCausallyLinked(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	in := dataEnvelope("ENV-1", "sess-1:client:text_out:1")
	local.toBridge <- in
	waitFor(t, func() bool { return len(local.emissions()) > 0 }, "no reply came back")

	got := local.emissions()[0]
	if got.CauseID != in.ID {
		t.Errorf("reply cause_id = %q; want %q so the chain stays walkable", got.CauseID, in.ID)
	}
	if got.Session != in.Session {
		t.Errorf("reply session = %q; want %q", got.Session, in.Session)
	}
	if !strings.HasPrefix(got.Idem, in.Idem) {
		t.Errorf("reply idem = %q; it must derive from %q", got.Idem, in.Idem)
	}
}

func TestUnreachableRemoteSurfacesAnErrorEnvelope(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)

	b := &Bridge{
		LocalWS: local.wsSkill(), LocalHTTP: local.srv.URL,
		RemoteHTTP: remote.httpURL(),
		RemoteWS:   "ws://127.0.0.1:1", // nothing listens here
		Log:        testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()
	select {
	case <-local.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy never registered")
	}

	local.toBridge <- dataEnvelope("ENV-1", "sess-1:client:text_out:1")
	waitFor(t, func() bool {
		for _, e := range local.emissions() {
			if e.Kind == channel.KindError {
				return true
			}
		}
		return false
	}, "a dead federation link produced no error envelope; the caller would wait forever")
}
