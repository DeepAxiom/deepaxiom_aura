package fed

import (
	"context"
	"encoding/json"
	"fmt"
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
		// Default: reply to `first`, then keep serving whatever further
		// envelopes arrive on the *same* connection — a real kernel's
		// /v1/stream never closes after one exchange, and the bridge's
		// pooled connection (bridge.go) relies on that being true. Every
		// reply carries CauseID: the real Session.forward() always does,
		// and the bridge's reply demux (pooledConn.readLoop) depends on it
		// exactly the way a real remote node's replies do.
		respond := func(in channel.Envelope) bool {
			// Echoes the incoming payload rather than a fixed string
			// deliberately: it's what lets a test prove a reply reached the
			// *right* concurrent caller (its own marker came back), not just
			// that some reply arrived (see TestConcurrentRelaysDoNotCrossReplies).
			if conn.WriteJSON(channel.Envelope{
				V: "1", ID: channel.NewID(), CauseID: in.ID, Kind: channel.KindData,
				Port: "text_out", Schema: "std/text@1", Payload: in.Payload,
			}) != nil {
				return false
			}
			return conn.WriteJSON(channel.Envelope{
				V: "1", ID: channel.NewID(), CauseID: in.ID, Kind: channel.KindDone,
			}) == nil
		}
		if !respond(first) {
			return
		}
		for {
			var env channel.Envelope
			if conn.ReadJSON(&env) != nil {
				return
			}
			n.mu.Lock()
			n.received = append(n.received, env)
			n.mu.Unlock()
			if env.Kind != channel.KindData {
				continue
			}
			if !respond(env) {
				return
			}
		}
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
//
// On a pooled connection (bridge.go), a cancel can no longer be "close the
// socket" — that would abort every other relay sharing it (see
// TestCancelDoesNotAffectAConcurrentRelayOnTheSameConnection just below) — so
// this now asserts the more precise thing the pool requires: a real `Kind:
// cancel` envelope, addressed with the cause_id the remote node was actually
// given for this specific relay.
func TestCancelCrossesTheBridge(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())

	cancelReceived := make(chan channel.Envelope, 1)
	stop := make(chan struct{})
	remote.onStream = func(t *testing.T, conn *websocket.Conn, _ channel.Envelope) {
		go func() {
			for {
				var env channel.Envelope
				if conn.ReadJSON(&env) != nil {
					return
				}
				if env.Kind == channel.KindCancel {
					select {
					case cancelReceived <- env:
					default:
					}
				}
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if conn.WriteJSON(channel.Envelope{
				V: "1", ID: channel.NewID(), Kind: channel.KindData,
				Port: "text_out", Schema: "std/text@1",
				Payload: json.RawMessage(`{"text":"tok","final":false}`),
			}) != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	local := newLocalStub(t)
	startBridge(t, local, remote)
	defer close(stop)

	in := dataEnvelope("ENV-1", "sess-1:client:text_out:1")
	local.toBridge <- in
	waitFor(t, func() bool { _, streams, _ := remote.snapshot(); return streams > 0 },
		"the bridge never opened a remote session")
	_, _, receivedByRemote := remote.snapshot()
	if len(receivedByRemote) == 0 {
		t.Fatal("the remote node never received the relayed envelope")
	}
	// What the remote actually knows this work by — not in.ID, which the
	// remote never sees at all.
	wantCauseID := receivedByRemote[len(receivedByRemote)-1].ID

	// The kernel addresses a cancel with the cause_id the proxy knows the work
	// by, which is the id of the envelope it was handed.
	local.toBridge <- channel.Envelope{
		V: "1", ID: channel.NewID(), CauseID: in.ID,
		Session: "sess-1", Kind: channel.KindCancel,
	}

	select {
	case got := <-cancelReceived:
		if got.CauseID != wantCauseID {
			t.Fatalf("cancel reached the remote with cause_id %q, want %q — it would not recognise this",
				got.CauseID, wantCauseID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancel did not reach the remote node")
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
	// Checked first and separately from the causal-metadata assertions below:
	// emitError() derives CauseID/Session/Idem exactly the same way emit()
	// does for a real reply, so those alone would still pass even if the
	// relay never actually got the remote's answer — Kind is what tells
	// apart "the reply came back" from "the relay gave up and reported why."
	if got.Kind != channel.KindData {
		t.Fatalf("first emission is Kind=%q, want data (payload=%s)", got.Kind, got.Payload)
	}
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

// --- the pooled connection (negotiated transport) ----------------------------

func markedEnvelope(id, idem, marker string) channel.Envelope {
	payload, _ := json.Marshal(map[string]any{"text": marker, "final": true})
	return channel.Envelope{
		V: "1", ID: id, Session: "sess-1", Node: "fedskill", Port: "text_in",
		Seq: 1, Idem: idem, Schema: "std/text@1", Kind: channel.KindData,
		Payload: payload,
	}
}

func payloadText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal payload %s: %v", raw, err)
	}
	return body.Text
}

// The concrete structural bottleneck this package's own doc comment used to
// accept: relay() dialing a brand-new WebSocket to the remote node for every
// single envelope. Five relays on the same capability must now open exactly
// one remote stream, not five — streamOpened is the same counter
// TestRemoteGraphIsRegisteredOncePerSession already uses to prove graph
// registration is cached; this proves the connection is too.
func TestRelaysOnTheSameCapabilityShareOneConnection(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	for i := 0; i < 5; i++ {
		local.toBridge <- dataEnvelope(channel.NewID(), fmt.Sprintf("sess-1:client:text_out:%d", i))
	}
	waitFor(t, func() bool { _, _, got := remote.snapshot(); return len(got) >= 5 },
		"not all envelopes were relayed")

	_, streams, _ := remote.snapshot()
	if streams != 1 {
		t.Fatalf("the remote stream was opened %d times for 5 relays on one capability; want 1", streams)
	}
}

// The adversarial case a demux keyed wrong, or racy under concurrent
// registration, would fail: fire several relays at once, each carrying a
// distinct marker the fake remote echoes straight back, and check every
// caller's own marker came back to it — not another concurrent caller's.
func TestConcurrentRelaysDoNotCrossReplies(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	const n = 8
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = channel.NewID()
		marker := fmt.Sprintf("marker-%d", i)
		local.toBridge <- markedEnvelope(ids[i], fmt.Sprintf("sess-1:client:text_out:%d", i), marker)
	}

	waitFor(t, func() bool {
		data := 0
		for _, e := range local.emissions() {
			if e.Kind == channel.KindData {
				data++
			}
		}
		return data >= n
	}, "not every relay received a reply")

	byLocalID := map[string]string{}
	for i, id := range ids {
		byLocalID[id] = fmt.Sprintf("marker-%d", i)
	}
	seen := map[string]bool{}
	for _, e := range local.emissions() {
		if e.Kind != channel.KindData {
			continue
		}
		wantMarker, ok := byLocalID[e.CauseID]
		if !ok {
			t.Fatalf("a data reply's cause_id %q does not match any local envelope this test sent", e.CauseID)
		}
		if got := payloadText(t, e.Payload); got != wantMarker {
			t.Fatalf("envelope %q got back marker %q, want its own %q — a reply crossed to the wrong caller",
				e.CauseID, got, wantMarker)
		}
		seen[e.CauseID] = true
	}
	if len(seen) != n {
		t.Fatalf("got replies for %d of %d relays", len(seen), n)
	}
}

// Cancelling one relay sharing a pooled connection must send a targeted
// cancel envelope, never close the shared socket — that would silently abort
// every other relay riding the same connection, the exact regression a naive
// port of the old close()-based cancel would introduce.
func TestCancelDoesNotAffectAConcurrentRelayOnTheSameConnection(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())

	held := make(chan channel.Envelope, 2)
	release := make(chan string, 2) // cause_id -> go ahead and reply
	remote.onStream = func(t *testing.T, conn *websocket.Conn, first channel.Envelope) {
		respond := func(in channel.Envelope) {
			_ = conn.WriteJSON(channel.Envelope{
				V: "1", ID: channel.NewID(), CauseID: in.ID, Kind: channel.KindData,
				Port: "text_out", Schema: "std/text@1", Payload: in.Payload,
			})
			_ = conn.WriteJSON(channel.Envelope{V: "1", ID: channel.NewID(), CauseID: in.ID, Kind: channel.KindDone})
		}
		pending := map[string]channel.Envelope{first.ID: first}
		held <- first
		go func() {
			for {
				var env channel.Envelope
				if conn.ReadJSON(&env) != nil {
					return
				}
				if env.Kind == channel.KindData {
					pending[env.ID] = env
					held <- env
				}
				// A cancel for one held request is simply never answered —
				// exactly what "stop working on this" should look like; it
				// must have no effect on any *other* pending request.
			}
		}()
		for id := range release {
			if in, ok := pending[id]; ok {
				respond(in)
			}
		}
	}

	local := newLocalStub(t)
	startBridge(t, local, remote)

	// a and b are dispatched by proxySession as two independent goroutines
	// (one per incoming data envelope), so there is no guarantee which one's
	// relay() reaches the shared connection first. Identify them by their
	// payload marker rather than by held's receive order, which is a race.
	a := markedEnvelope(channel.NewID(), "sess-1:client:text_out:a", "marker-a")
	b := markedEnvelope(channel.NewID(), "sess-1:client:text_out:b", "marker-b")
	local.toBridge <- a
	local.toBridge <- b

	remoteByMarker := map[string]string{}
	for i := 0; i < 2; i++ {
		env := <-held
		remoteByMarker[payloadText(t, env.Payload)] = env.ID
	}
	remoteBID, ok := remoteByMarker["marker-b"]
	if !ok {
		t.Fatalf("never saw B's envelope reach the remote (got markers: %v)", remoteByMarker)
	}

	// Cancel A only.
	local.toBridge <- channel.Envelope{
		V: "1", ID: channel.NewID(), CauseID: a.ID, Session: "sess-1", Kind: channel.KindCancel,
	}
	// Give the cancel a moment to land before letting B proceed, so a bug
	// that tore down the shared connection on any cancel has a chance to show.
	time.Sleep(100 * time.Millisecond)
	release <- remoteBID

	waitFor(t, func() bool {
		for _, e := range local.emissions() {
			if e.Kind == channel.KindDone && e.CauseID == b.ID {
				return true
			}
		}
		return false
	}, "the uncancelled relay (B) never completed — cancelling A affected the shared connection")

	for _, e := range local.emissions() {
		if e.CauseID == a.ID && (e.Kind == channel.KindData || e.Kind == channel.KindDone) {
			t.Fatalf("the cancelled relay (A) still produced a reply: %+v", e)
		}
	}
}

// A pooled connection that dies must not permanently break the capability —
// the next relay reconnects, the same self-healing principle serveProxy
// already applies one level up (reconnecting the whole proxy session).
func TestPooledConnectionSelfHealsAfterADrop(t *testing.T) {
	remote := newFakeNode(t, remoteCatalog())
	local := newLocalStub(t)
	startBridge(t, local, remote)

	local.toBridge <- dataEnvelope("ENV-1", "sess-1:client:text_out:1")
	waitFor(t, func() bool { return len(local.emissions()) > 0 }, "first relay never replied")

	// The fake remote's default handler returns (closing its side) after
	// answering — simulating the pooled connection dying between uses.
	waitFor(t, func() bool { _, streams, _ := remote.snapshot(); return streams >= 1 },
		"remote never opened a stream")

	local.toBridge <- dataEnvelope("ENV-2", "sess-1:client:text_out:2")
	waitFor(t, func() bool {
		for _, e := range local.emissions() {
			if e.CauseID == "ENV-2" && e.Kind == channel.KindData {
				return true
			}
		}
		return false
	}, "the second relay did not recover after the pooled connection died")
}

// --- route classification -----------------------------------------------------

func TestClassifyRoute(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		rtt  time.Duration
		want RouteClass
	}{
		{"loopback IP is same-host regardless of rtt", "http://127.0.0.1:9080", 200 * time.Millisecond, RouteSameHost},
		{"localhost hostname is same-host", "http://localhost:9080", 50 * time.Millisecond, RouteSameHost},
		{"fast remote is lan", "http://10.0.0.5:9080", 5 * time.Millisecond, RouteLAN},
		{"right at the threshold is lan", "http://10.0.0.5:9080", 15 * time.Millisecond, RouteLAN},
		{"slow remote is relay", "http://example.com:9080", 200 * time.Millisecond, RouteRelay},
		{"just past the threshold is relay", "http://10.0.0.5:9080", 16 * time.Millisecond, RouteRelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRoute(tc.url, tc.rtt); got != tc.want {
				t.Errorf("classifyRoute(%q, %s) = %q, want %q", tc.url, tc.rtt, got, tc.want)
			}
		})
	}
}
