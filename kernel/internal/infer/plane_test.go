package infer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// engine is a stand-in for an OpenAI-compatible server. It records what it
// was sent and answers with a stream the test can hold open.
type engine struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string

	// refuseStreamOptions makes it answer 400 to a body carrying the field,
	// as an engine that predates it does.
	refuseStreamOptions bool
	// hold, when set, is waited on after the first token: the stream stays
	// open until the test releases it or the request is cancelled.
	hold chan struct{}
	// started is signalled once the first token of a stream has been written.
	started chan struct{}
	// gone receives once per request whose context was cancelled before the
	// engine finished — which is what "the model was released" looks like
	// from the engine's side.
	gone chan struct{}
}

func newEngine(t *testing.T) *engine {
	t.Helper()
	e := &engine{t: t, started: make(chan struct{}, 16), gone: make(chan struct{}, 16)}
	e.srv = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *engine) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	e.bodies = append(e.bodies, raw)
	e.headers = append(e.headers, r.Header.Clone())
	e.paths = append(e.paths, r.URL.Path)
	refuse, hold := e.refuseStreamOptions, e.hold
	e.mu.Unlock()

	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"test-model"}]}`)
		return
	}
	var req struct {
		Stream        bool            `json:"stream"`
		StreamOptions json.RawMessage `json:"stream_options"`
	}
	_ = json.Unmarshal(raw, &req)
	if refuse && len(req.StreamOptions) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"unknown field stream_options"}}`)
		return
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],`+
			`"usage":{"prompt_tokens":30,"completion_tokens":4}}`)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	_, _ = io.WriteString(w, evRole+evHello)
	flusher.Flush()
	e.started <- struct{}{}
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			e.gone <- struct{}{}
			return
		}
	}
	_, _ = io.WriteString(w, evLo+evFinish)
	if asksForUsage(req.StreamOptions) {
		_, _ = io.WriteString(w, evUsage)
	}
	_, _ = io.WriteString(w, evDone)
}

func (e *engine) lastBody() map[string]json.RawMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.bodies) == 0 {
		e.t.Fatal("the engine was never called")
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(e.bodies[len(e.bodies)-1], &out); err != nil {
		e.t.Fatalf("the engine was sent something that is not JSON: %v", err)
	}
	return out
}

func (e *engine) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.bodies)
}

// node mounts a plane the way the gateway does and lets a test play caller.
type node struct {
	t     *testing.T
	plane *Plane
	srv   *httptest.Server
	who   Who
	clock *time.Time
}

func newNode(t *testing.T, e *engine, cfg Config, resolve Resolver) *node {
	t.Helper()
	n := &node{t: t, who: Who{Operator: true}}
	clock := time.UnixMilli(1_800_000_000_000)
	n.clock = &clock
	cfg.Upstream = e.srv.URL + "/v1"
	cfg.Version = "test"
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return *n.clock }
	}
	p, err := New(cfg, resolve)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.plane = p
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Serve(w, r, n.who, strings.TrimPrefix(r.URL.Path, "/v1/inference/"))
	}))
	t.Cleanup(n.srv.Close)
	return n
}

const chatBody = `{"model":"m","messages":[{"role":"user","content":"a < b"}],"stream":true}`

func (n *node) post(route, body string, headers map[string]string) *http.Response {
	n.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, n.srv.URL+"/v1/inference/"+route, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer the-callers-own-node-token")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatalf("POST %s: %v", route, err)
	}
	return resp
}

func read(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(read(t, resp)), &body)
	return body.Error.Code
}

func graphStats(t *testing.T, p *Plane, graph string) GraphStats {
	t.Helper()
	for _, g := range p.Stats().Graphs {
		if g.Graph == graph {
			return g
		}
	}
	t.Fatalf("no row for graph %q in %+v", graph, p.Stats().Graphs)
	return GraphStats{}
}

func delivery(session, id string) map[string]string {
	return map[string]string{HeaderSession: session, HeaderDelivery: id, HeaderNode: "brain", HeaderPort: "text_in"}
}

func resolveTo(res Resolved) Resolver {
	return func(Ref, Who) (Resolved, error) { return res, nil }
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

// ---------------------------------------------------------------------------

func TestAStreamPassesThroughAndIsCounted(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{}, nil)

	resp := n.post("chat/completions", chatBody, nil)
	got := read(t, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get(HeaderRequest) == "" {
		t.Fatal("the response does not say which request the node knows it by")
	}
	// The caller did not ask for usage, so it must see exactly what an engine
	// asked for none would have sent — though the node did ask.
	if want := evRole + evHello + evLo + evFinish + evDone; got != want {
		t.Fatalf("the caller's stream:\n got %q\nwant %q", got, want)
	}
	if !asksForUsage(e.lastBody()["stream_options"]) {
		t.Fatal("the node did not ask the engine for usage")
	}

	g := graphStats(t, n.plane, Unattributed)
	if g.Requests != 1 || g.Completed != 1 || g.Metered != 1 {
		t.Fatalf("counted %+v", g)
	}
	if g.InputTokens != 120 || g.OutputTokens != 2 || g.CachedInputTokens != 100 {
		t.Fatalf("tokens %d/%d/%d, want 120/2/100", g.InputTokens, g.OutputTokens, g.CachedInputTokens)
	}
	if g.FirstToken.Count != 1 {
		t.Fatalf("first-token samples: %d", g.FirstToken.Count)
	}
}

func TestACallerThatAskedForUsageGetsItsUsageChunk(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{}, nil)
	body := `{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`
	got := read(t, n.post("chat/completions", body, nil))
	if !strings.Contains(got, `"prompt_tokens":120`) {
		t.Fatalf("a caller that asked for usage did not receive it: %q", got)
	}
}

// What the node says a model was shown has to be what the model was sent.
func TestMessagesReachTheEngineByteForByte(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{Priority: PriorityVLLM},
		resolveTo(Resolved{Session: "s", Graph: "g", Priority: 3, Known: true}))
	messages := `[ {"role":"user","content":"a < b && c > d  é é \/ "} ]`
	body := `{ "priority": 99, "model":"m", "messages":` + messages + `, "stream":true }`
	read(t, n.post("chat/completions", body, delivery("s", "d1")))

	e.mu.Lock()
	sent := e.bodies[len(e.bodies)-1]
	e.mu.Unlock()
	if !bytes.Contains(sent, []byte(messages)) {
		t.Fatalf("the engine did not receive messages as written:\n%s", sent)
	}
}

func TestTheNodeStatesThePriorityNotTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name       string
		convention string
		resolved   *Resolved
		want       string // "" = the field must be absent
	}{
		{"vllm serves the lowest first", PriorityVLLM, &Resolved{Session: "s", Graph: "g", Priority: 7, Known: true}, "-7"},
		{"sglang serves the highest first", PrioritySGLang, &Resolved{Session: "s", Graph: "g", Priority: 7, Known: true}, "7"},
		{"an engine with no priorities gets none", PriorityNone, &Resolved{Session: "s", Graph: "g", Priority: 7, Known: true}, ""},
		{"ordinary priority is sent as nothing", PriorityVLLM, &Resolved{Session: "s", Graph: "g", Known: true}, ""},
		{"an unattributed request has no priority to state", PriorityVLLM, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t)
			var resolve Resolver
			var headers map[string]string
			if tc.resolved != nil {
				resolve, headers = resolveTo(*tc.resolved), delivery("s", "d1")
			}
			n := newNode(t, e, Config{Priority: tc.convention}, resolve)
			// The caller names its own urgency. It does not get to.
			body := `{"model":"m","messages":[],"stream":true,"priority":-1000}`
			read(t, n.post("chat/completions", body, headers))

			got, present := e.lastBody()["priority"]
			if tc.want == "" && present {
				t.Fatalf("the engine was sent priority %s", got)
			}
			if tc.want != "" && string(got) != tc.want {
				t.Fatalf("the engine was sent priority %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTheCallersCredentialStopsAtTheNode(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{APIKey: "engine-key"}, nil)
	read(t, n.post("chat/completions", chatBody, delivery("s", "d1")))

	e.mu.Lock()
	h := e.headers[len(e.headers)-1]
	e.mu.Unlock()
	if got := h.Get("Authorization"); got != "Bearer engine-key" {
		t.Fatalf("the engine was sent Authorization %q", got)
	}
	for _, name := range []string{HeaderSession, HeaderDelivery, HeaderNode, HeaderPort} {
		if h.Get(name) != "" {
			t.Fatalf("%s was forwarded to the engine", name)
		}
	}

	// And with no engine key, none at all — never the caller's.
	e2 := newEngine(t)
	n2 := newNode(t, e2, Config{}, nil)
	read(t, n2.post("chat/completions", chatBody, nil))
	e2.mu.Lock()
	defer e2.mu.Unlock()
	if got := e2.headers[0].Get("Authorization"); got != "" {
		t.Fatalf("a node token reached the engine: %q", got)
	}
}

func TestRequestsAreCountedUnderTheGraphTheyBelongTo(t *testing.T) {
	e := newEngine(t)
	var seen Ref
	var asked Who
	n := newNode(t, e, Config{}, func(ref Ref, who Who) (Resolved, error) {
		seen, asked = ref, who
		return Resolved{Session: ref.Session, Graph: "voice", Known: true}, nil
	})
	n.who = Who{TokenID: "tok-1", Capability: "cognitive.llm.chat"}
	read(t, n.post("chat/completions", chatBody, delivery("sess-9", "deliv-3")))

	if seen != (Ref{Session: "sess-9", Delivery: "deliv-3", Node: "brain", Port: "text_in"}) {
		t.Fatalf("the executor was asked about %+v", seen)
	}
	if asked != n.who {
		t.Fatalf("the executor was told the caller is %+v", asked)
	}
	if g := graphStats(t, n.plane, "voice"); g.Requests != 1 || g.OutputTokens != 2 {
		t.Fatalf("voice: %+v", g)
	}
}

func TestARequestTheNodeCannotStandBehindIsRefusedBeforeTheEngine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resolve Resolver
		status  int
		code    string
	}{
		{"a delivery made to someone else", func(Ref, Who) (Resolved, error) { return Resolved{}, ErrForeign },
			http.StatusForbidden, "not_your_delivery"},
		{"a delivery the node cannot place", func(Ref, Who) (Resolved, error) { return Resolved{}, ErrUnplaced },
			http.StatusConflict, "unknown_delivery"},
		{"a chain already cancelled", resolveTo(Resolved{Session: "s", Graph: "g", Known: true, Cancelled: true}),
			http.StatusConflict, "cancelled"},
		{"a deadline already past", resolveTo(Resolved{Session: "s", Graph: "g", Known: true, Deadline: 1_799_999_999_000}),
			http.StatusRequestTimeout, "deadline_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t)
			n := newNode(t, e, Config{}, tc.resolve)
			resp := n.post("chat/completions", chatBody, delivery("s", "d1"))
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.status)
			}
			if code := errorCode(t, resp); code != tc.code {
				t.Fatalf("error code %q, want %q", code, tc.code)
			}
			if e.calls() != 0 {
				t.Fatal("the engine was asked to work on a request the node refused")
			}
			if n.plane.Stats().Refused != 1 {
				t.Fatalf("refused counter: %d", n.plane.Stats().Refused)
			}
		})
	}
}

func TestBadBodiesAreRefusedWithTheReason(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{MaxBodyBytes: 256}, nil)
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"a key written twice": {`{"priority":0,"messages":[],"priority":9}`, 400, "bad_request"},
		"not an object":       {`[1,2,3]`, 400, "bad_request"},
		"not json":            {`{"messages":`, 400, "bad_request"},
		"too large":           {`{"messages":"` + strings.Repeat("x", 400) + `"}`, 413, "body_too_large"},
	} {
		resp := n.post("chat/completions", tc.body, nil)
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: status %d, want %d", name, resp.StatusCode, tc.status)
		}
		if code := errorCode(t, resp); code != tc.code {
			t.Fatalf("%s: error code %q, want %q", name, code, tc.code)
		}
	}
	if e.calls() != 0 {
		t.Fatal("a malformed body reached the engine")
	}
}

// An engine that predates `stream_options` answers 400. The node asks again
// as the caller did, and does not ask for usage a second time.
func TestAnEngineThatRefusesStreamOptionsIsAskedWithoutThem(t *testing.T) {
	e := newEngine(t)
	e.refuseStreamOptions = true
	n := newNode(t, e, Config{}, nil)

	first := n.post("chat/completions", chatBody, nil)
	if got := read(t, first); first.StatusCode != 200 || !strings.Contains(got, "[DONE]") {
		t.Fatalf("status %d, body %q", first.StatusCode, got)
	}
	if e.calls() != 2 {
		t.Fatalf("the engine was called %d times for the first request, want 2 (refused, then retried)", e.calls())
	}
	if _, present := e.lastBody()["stream_options"]; present {
		t.Fatal("the retry still carried stream_options")
	}
	if n.plane.Stats().UsageRequested {
		t.Fatal("the node did not remember that this engine refuses stream_options")
	}

	read(t, n.post("chat/completions", chatBody, nil))
	if e.calls() != 3 {
		t.Fatalf("the second request cost %d engine calls, want 1", e.calls()-2)
	}
	// Nothing was reported, so nothing is counted: not a guess.
	if g := graphStats(t, n.plane, Unattributed); g.Metered != 0 || g.InputTokens != 0 || g.Completed != 2 {
		t.Fatalf("counted %+v", g)
	}
}

func TestANonStreamedRequestIsRelayedAndMetered(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{}, nil)
	resp := n.post("chat/completions", `{"model":"m","messages":[]}`, nil)
	if got := read(t, resp); !strings.Contains(got, `"content":"hi"`) {
		t.Fatalf("body %q", got)
	}
	if _, present := e.lastBody()["stream_options"]; present {
		t.Fatal("stream_options was added to a request that does not stream")
	}
	if g := graphStats(t, n.plane, Unattributed); g.InputTokens != 30 || g.OutputTokens != 4 {
		t.Fatalf("counted %+v", g)
	}
}

func TestEveryRouteGoesToItsCounterpartOnTheEngine(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{}, nil)
	for _, route := range []string{"chat/completions", "completions", "embeddings", "rerank"} {
		read(t, n.post(route, `{"model":"m","input":"x"}`, nil))
	}
	resp, err := http.Get(n.srv.URL + "/v1/inference/models")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, resp); !strings.Contains(got, "test-model") {
		t.Fatalf("models: %q", got)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	want := []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/rerank", "/v1/models"}
	if strings.Join(e.paths, " ") != strings.Join(want, " ") {
		t.Fatalf("the engine was asked for %v, want %v", e.paths, want)
	}
	// And nothing else is a route.
	if resp := n.post("audio/speech", `{}`, nil); resp.StatusCode != 404 {
		t.Fatalf("an unknown route answered %d", resp.StatusCode)
	}
}

// --- stopping --------------------------------------------------------------

// open starts a stream and returns once the engine has sent its first token
// and is holding the rest.
func (n *node) open(e *engine, headers map[string]string) (*http.Response, chan string) {
	n.t.Helper()
	resp := n.post("chat/completions", chatBody, headers)
	select {
	case <-e.started:
	case <-time.After(5 * time.Second):
		n.t.Fatal("the engine never started the stream")
	}
	done := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- string(raw)
	}()
	return resp, done
}

func released(t *testing.T, e *engine, what string) {
	t.Helper()
	select {
	case <-e.gone:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s, and the engine was left generating", what)
	}
}

func ended(t *testing.T, done chan string) string {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("the caller's stream was never closed")
		return ""
	}
}

// Cancellation was a message to the Expertise and a hope that it noticed. The
// node now closes the request at the engine itself.
func TestACancelledChainIsCutAtTheEngine(t *testing.T) {
	e := newEngine(t)
	e.hold = make(chan struct{})
	n := newNode(t, e, Config{},
		resolveTo(Resolved{Session: "s", Graph: "g", Root: "root-1", Cause: "cause-1", Known: true}))
	_, done := n.open(e, delivery("s", "d1"))

	// Another session's cancel, and another chain's, are not this request's.
	if hit := n.plane.Cancel("other", "root-1", "", StopChain, "x"); hit != 0 {
		t.Fatalf("a cancel for another session ended %d request(s)", hit)
	}
	if hit := n.plane.Cancel("s", "root-2", "", StopChain, "x"); hit != 0 {
		t.Fatalf("a cancel for another chain ended %d request(s)", hit)
	}
	if hit := n.plane.Cancel("s", "root-1", "", StopChain, "the chain was cancelled"); hit != 1 {
		t.Fatalf("the cancel ended %d request(s), want 1", hit)
	}
	released(t, e, "a chain was cancelled")
	if got := ended(t, done); strings.Contains(got, "[DONE]") {
		t.Fatalf("a cancelled stream ran to its end: %q", got)
	}
	waitFor(t, "the request to be counted", func() bool { return graphStats(t, n.plane, "g").Cancelled == 1 })
	if n.plane.Stats().Stopped[StopChain] != 1 {
		t.Fatalf("stopped: %+v", n.plane.Stats().Stopped)
	}
	if n.plane.InFlight() != 0 {
		t.Fatalf("%d request(s) still tracked", n.plane.InFlight())
	}
}

func TestOneDeliveryIsCutByItsCauseAndASessionByItsEnd(t *testing.T) {
	e := newEngine(t)
	e.hold = make(chan struct{})
	n := newNode(t, e, Config{},
		resolveTo(Resolved{Session: "s", Graph: "g", Root: "root-1", Cause: "cause-1", Known: true}))

	_, done := n.open(e, delivery("s", "d1"))
	if hit := n.plane.Cancel("s", "", "cause-1", StopSpeculation, "superseded"); hit != 1 {
		t.Fatalf("a cancel by cause ended %d request(s)", hit)
	}
	released(t, e, "a speculation was superseded")
	ended(t, done)

	_, done = n.open(e, delivery("s", "d2"))
	if hit := n.plane.Cancel("s", "", "", StopSession, "the session ended"); hit != 1 {
		t.Fatalf("a session ending ended %d request(s)", hit)
	}
	released(t, e, "a session ended")
	ended(t, done)

	stopped := n.plane.Stats().Stopped
	waitFor(t, "both stops to be counted", func() bool {
		stopped = n.plane.Stats().Stopped
		return stopped[StopSpeculation] == 1 && stopped[StopSession] == 1
	})
}

func TestADeadlineCutsTheRequestWhenItPasses(t *testing.T) {
	e := newEngine(t)
	e.hold = make(chan struct{})
	// The real clock here: the cut is a timer, not a comparison.
	soon := time.Now().Add(300 * time.Millisecond).UnixMilli()
	n := newNode(t, e, Config{Now: time.Now},
		resolveTo(Resolved{Session: "s", Graph: "g", Root: "r", Known: true, Deadline: soon}))
	_, done := n.open(e, delivery("s", "d1"))

	released(t, e, "a deadline passed")
	ended(t, done)
	waitFor(t, "the stop to be counted", func() bool { return n.plane.Stats().Stopped[StopDeadline] == 1 })
}

func TestACallerThatLeavesReleasesTheEngine(t *testing.T) {
	e := newEngine(t)
	e.hold = make(chan struct{})
	n := newNode(t, e, Config{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		n.srv.URL+"/v1/inference/chat/completions", strings.NewReader(chatBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-e.started
	cancel()
	resp.Body.Close()

	released(t, e, "the caller hung up")
	waitFor(t, "the stop to be counted", func() bool { return n.plane.Stats().Stopped[StopCaller] == 1 })
}

// A node stopped during a generation must not wait for the model.
func TestShutdownCutsWhatIsInFlightAndRefusesWhatComesNext(t *testing.T) {
	e := newEngine(t)
	e.hold = make(chan struct{})
	n := newNode(t, e, Config{}, nil)
	_, done := n.open(e, nil)

	began := time.Now()
	if hit := n.plane.Shutdown(); hit != 1 {
		t.Fatalf("shutdown ended %d request(s), want 1", hit)
	}
	released(t, e, "the node was stopping")
	ended(t, done)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("a stream held open by the engine took %v to end at shutdown", took)
	}
	waitFor(t, "the handler to return", func() bool { return n.plane.InFlight() == 0 })

	resp := n.post("chat/completions", chatBody, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a request after shutdown answered %d", resp.StatusCode)
	}
	read(t, resp)
}

func TestAnUnreachableEngineIsABadGatewayNotAHang(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{}, nil)
	e.srv.Close()
	resp := n.post("chat/completions", chatBody, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "engine_unreachable" {
		t.Fatalf("code %q", code)
	}
	if g := graphStats(t, n.plane, Unattributed); g.Failed != 1 {
		t.Fatalf("counted %+v", g)
	}
}

func TestAnEnginesOwnErrorIsRelayedAndCountedAsFailed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"message":"context length exceeded"}}`)
	}))
	defer upstream.Close()
	p, err := New(Config{Upstream: upstream.URL + "/v1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/inference/chat/completions", strings.NewReader(`{"messages":[]}`))
	p.Serve(rec, req, Who{Operator: true}, "chat/completions")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "context length") {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if g := graphStats(t, p, Unattributed); g.Failed != 1 || g.Completed != 0 {
		t.Fatalf("counted %+v", g)
	}
}

// --- configuration and bounds ----------------------------------------------

func TestConfigurationIsCheckedWhenTheNodeStarts(t *testing.T) {
	for _, cfg := range []Config{
		{Upstream: ""},
		{Upstream: "not a url"},
		{Upstream: "ftp://engine/v1"},
		{Upstream: "http://engine/v1", Priority: "urgent-first"},
	} {
		if _, err := New(cfg, nil); err == nil {
			t.Fatalf("%+v was accepted", cfg)
		}
	}
	p, err := New(Config{Upstream: "http://user:secret@engine:8000/v1/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Upstream(), "secret") {
		t.Fatalf("the engine's credentials are in what the node reports: %s", p.Upstream())
	}
}

func TestGraphRowsAreBounded(t *testing.T) {
	var c counters
	for i := 0; i < maxGraphs+50; i++ {
		c.graph("g" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i/676))).requests.Add(1)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.graphs) > maxGraphs+1 {
		t.Fatalf("%d rows for a bound of %d", len(c.graphs), maxGraphs)
	}
	if c.graphs[OtherGraphs] == nil || c.graphs[OtherGraphs].requests.Load() == 0 {
		t.Fatal("requests past the bound were not counted anywhere")
	}
}

func TestTheFirstTokenHistogramPutsASampleInItsBucket(t *testing.T) {
	var h histogram
	h.observe(10 * time.Millisecond)
	h.observe(25 * time.Millisecond) // on a bound: belongs to it
	h.observe(26 * time.Millisecond) // just over: the next
	h.observe(time.Hour)             // above every bound
	h.observe(-5 * time.Millisecond) // a clock that went backwards
	s := h.snapshot()
	if s.Count != 5 {
		t.Fatalf("count %d", s.Count)
	}
	if s.Buckets[0] != 3 || s.Buckets[1] != 1 || s.Buckets[len(s.Buckets)-1] != 1 {
		t.Fatalf("buckets %v", s.Buckets)
	}
	if len(s.Buckets) != len(s.BoundsMS)+1 {
		t.Fatalf("%d buckets for %d bounds", len(s.Buckets), len(s.BoundsMS))
	}
}

func TestTheEngineIsAskedWhatItServes(t *testing.T) {
	e := newEngine(t)
	n := newNode(t, e, Config{Now: time.Now}, nil)
	if n.plane.Stats().Engine != nil {
		t.Fatal("the node claims to know its engine before asking it")
	}
	info := n.plane.Probe(context.Background())
	if !info.Reachable || len(info.Models) != 1 || info.Models[0] != "test-model" {
		t.Fatalf("probe: %+v", info)
	}
	if got := n.plane.Stats().Engine; got == nil || !got.Reachable {
		t.Fatalf("stats do not carry what the probe found: %+v", got)
	}

	e.srv.Close()
	if info := n.plane.Probe(context.Background()); info.Reachable || info.Error == "" {
		t.Fatalf("a closed engine was reported as %+v", info)
	}
}

func TestGuardKeepsAPanicFromLeaving(t *testing.T) {
	p, err := New(Config{Upstream: "http://engine/v1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Guard("test", func() { panic(errors.New("boom")) }) // must return
}
