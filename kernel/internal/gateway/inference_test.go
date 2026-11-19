package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/executor"
	"aura/kernel/internal/infer"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
)

// What is tested here is the join: a credential presented to the node's HTTP
// surface becoming a party the executor recognises, and the executor's notice
// that a chain was cancelled becoming a closed request at the engine. The
// plane's own behaviour is covered in internal/infer, and the executor's
// answers in internal/executor.

func TestInferenceRoutesAreOpenToASkillTokenAndNothingElseIs(t *testing.T) {
	a := testAuth()
	for _, path := range []string{
		"/v1/inference/chat/completions", "/v1/inference/embeddings", "/v1/inference/rerank",
	} {
		if code, p := probe(t, a, http.MethodPost, path, skillToken); code != http.StatusOK || p.Scope != ScopeSkill {
			t.Fatalf("a skill token on %s: status %d, principal %+v", path, code, p)
		}
	}
	if code, _ := probe(t, a, http.MethodGet, "/v1/inference/models", skillToken); code != http.StatusOK {
		t.Fatalf("a skill token cannot list its node's models: %d", code)
	}
	// The grant is that prefix and no wider.
	for _, path := range []string{"/v1/inferenceX", "/v1/graphs", "/v1/ledger", "/v1/stats"} {
		if code, _ := probe(t, a, http.MethodGet, path, skillToken); code != http.StatusForbidden {
			t.Fatalf("a skill token reached %s (%d)", path, code)
		}
	}
	// And it is authenticated like every other route: no credential, no model.
	if code, _ := probe(t, a, http.MethodPost, "/v1/inference/chat/completions", ""); code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated caller reached the inference route (%d)", code)
	}
}

func TestANodeWithNoEngineSaysSoRatherThanServingTheAppShell(t *testing.T) {
	g := &Gateway{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/inference/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code, Message string
		}
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode != http.StatusNotFound || body.Error.Code != "no_engine" {
		t.Fatalf("status %d, body %s", resp.StatusCode, raw)
	}
	if !strings.Contains(body.Error.Message, "--inference-url") {
		t.Fatalf("the answer does not say how to turn it on: %q", body.Error.Message)
	}
}

// fakeEngine streams one token and then holds until the request is cancelled.
type fakeEngine struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  []map[string]json.RawMessage
	started chan struct{}
	gone    chan struct{}
}

func newFakeEngine(t *testing.T, hold bool) *fakeEngine {
	t.Helper()
	e := &fakeEngine{started: make(chan struct{}, 8), gone: make(chan struct{}, 8)}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		e.mu.Lock()
		e.bodies = append(e.bodies, body)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		e.started <- struct{}{}
		if hold {
			<-r.Context().Done()
			e.gone <- struct{}{}
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":1}}`+"\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// inferenceNode is a gateway with a live session, one Expertise wired into it
// under a scoped token, and an inference plane in front of a fake engine.
type inferenceNode struct {
	srv      *httptest.Server
	mgr      *executor.Manager
	plane    *infer.Plane
	sess     *executor.Session
	delivery channel.Envelope
	message  channel.Envelope
}

const (
	brainToken  = "brain-token-value"
	otherToken  = "other-token-value"
	brainCap    = "cognitive.test.brain"
	otherCap    = "cognitive.test.other"
	sessionName = "sess-infer"
)

func newInferenceNode(t *testing.T, e *fakeEngine, priority string) *inferenceNode {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg := registry.New()

	var delivered []channel.Envelope
	var mu sync.Mutex
	register := func(id, capability, tokenID string) {
		m := registry.Manifest{
			ID: id, Version: "1.0.0", Protocol: "1", Name: id, Description: "test",
			Capability: capability, Type: "cognitive", Format: "source",
		}
		m.Ports.Ingress = []registry.Port{{Name: "text_in", Schema: "std/text@1"}}
		m.Ports.Egress = []registry.Port{{Name: "text_out", Schema: "std/text@1"}}
		reg.Register(id, &registry.Live{Manifest: m, TokenID: tokenID,
			Send: func(raw []byte, _ string) error {
				var env channel.Envelope
				_ = json.Unmarshal(raw, &env)
				mu.Lock()
				delivered = append(delivered, env)
				mu.Unlock()
				return nil
			}})
	}
	register("acme/cognitive/brain", brainCap, "tok-brain")
	register("acme/cognitive/other", otherCap, "tok-other")

	graph, _ := json.Marshal(map[string]any{
		"ir": "1", "graph_id": "assistant", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{
			{"ref": "brain", "resolve": brainCap},
			{"ref": "other", "resolve": otherCap},
		},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "brain.text_in", "priority": 6},
			{"from": "brain.text_out", "to": "client.text_in"},
			{"from": "other.text_out", "to": "client.text_in"},
		},
	})
	if err := st.SaveGraph("assistant", graph); err != nil {
		t.Fatal(err)
	}
	mgr := executor.NewManager(reg, st, "local", executor.DefaultPolicy(), nil, log)
	plane, err := infer.New(infer.Config{Upstream: e.srv.URL + "/v1", Priority: priority, Log: log},
		InferenceResolver(mgr))
	if err != nil {
		t.Fatal(err)
	}
	ConnectInference(mgr, plane)

	auth := &Auth{Token: operatorToken, Tokens: fakeTokens{
		HashToken(brainToken): {ID: "tok-brain", Scope: string(ScopeSkill), Capability: brainCap},
		HashToken(otherToken): {ID: "tok-other", Scope: string(ScopeSkill), Capability: otherCap},
	}}
	g := &Gateway{Reg: reg, St: st, Mgr: mgr, Infer: plane, Auth: auth, Log: log}
	n := &inferenceNode{mgr: mgr, plane: plane}
	n.srv = httptest.NewServer(g.Handler())
	t.Cleanup(n.srv.Close)

	sess, err := mgr.Start(sessionName, "assistant", "", func([]byte, string) error { return nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { mgr.End(sessionName) })
	n.sess = sess

	payload, _ := json.Marshal(map[string]any{"text": "hello", "final": true})
	n.message = channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: sessionName,
		Node: executor.ClientRef, Port: "text_out", Seq: 1, Idem: channel.NewID(),
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	}
	sess.Route(n.message)
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 {
		t.Fatalf("precondition: %d deliveries to the Expertise, want 1", len(delivered))
	}
	n.delivery = delivered[0]
	return n
}

func (n *inferenceNode) request(t *testing.T, token string) *http.Request {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, n.srv.URL+"/v1/inference/chat/completions",
		strings.NewReader(`{"model":"m","messages":[],"stream":true,"priority":-999}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(infer.HeaderSession, sessionName)
	req.Header.Set(infer.HeaderDelivery, n.delivery.ID)
	req.Header.Set(infer.HeaderNode, "brain")
	req.Header.Set(infer.HeaderPort, "text_in")
	return req
}

// The whole point, end to end inside one process: an Expertise's request,
// under its own narrow token, is placed in its graph and sent to the engine
// with the priority the *edge* declared — not the one the request claimed.
func TestAnExpertisesRequestIsPlacedByTheNodeNotByItself(t *testing.T) {
	e := newFakeEngine(t, false)
	n := newInferenceNode(t, e, infer.PriorityVLLM)

	resp, err := http.DefaultClient.Do(n.request(t, brainToken))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}

	e.mu.Lock()
	sent := e.bodies[0]
	e.mu.Unlock()
	if got := string(sent["priority"]); got != "-6" {
		t.Fatalf("the engine was sent priority %s; the edge declares 6, which vLLM spells -6, "+
			"and the request's own -999 is not the Expertise's to state", got)
	}

	stats := n.plane.Stats()
	if len(stats.Graphs) != 1 || stats.Graphs[0].Graph != "assistant" {
		t.Fatalf("counted under %+v, want the graph the session is running", stats.Graphs)
	}
	if stats.Graphs[0].InputTokens != 11 || stats.Graphs[0].OutputTokens != 1 {
		t.Fatalf("tokens %+v", stats.Graphs[0])
	}
}

// A second Expertise naming the first one's delivery, to borrow its priority.
func TestOneExpertiseCannotSpeakForAnothersDelivery(t *testing.T) {
	e := newFakeEngine(t, false)
	n := newInferenceNode(t, e, infer.PriorityVLLM)

	resp, err := http.DefaultClient.Do(n.request(t, otherToken))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(raw), "not_your_delivery") {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.bodies) != 0 {
		t.Fatal("the engine served a request made under someone else's delivery")
	}
}

// Cancelling used to be a message to the Expertise. Here it is the client's
// cancel, routed through the real executor, closing the request at the engine.
func TestAClientsCancelClosesTheRequestAtTheEngine(t *testing.T) {
	e := newFakeEngine(t, true)
	n := newInferenceNode(t, e, infer.PriorityNone)

	resp, err := http.DefaultClient.Do(n.request(t, brainToken))
	if err != nil {
		t.Fatal(err)
	}
	<-e.started
	done := make(chan struct{})
	go func() { _, _ = io.ReadAll(resp.Body); resp.Body.Close(); close(done) }()

	n.sess.Route(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: n.message.ID,
		Session: sessionName, Kind: channel.KindCancel,
	})
	<-e.gone
	<-done
	if got := n.plane.Stats().Stopped[infer.StopChain]; got != 1 {
		t.Fatalf("stopped: %+v", n.plane.Stats().Stopped)
	}

	// And a request arriving for that chain afterwards is turned away.
	late, err := http.DefaultClient.Do(n.request(t, brainToken))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(late.Body)
	late.Body.Close()
	if late.StatusCode != http.StatusConflict || !strings.Contains(string(raw), `"cancelled"`) {
		t.Fatalf("a request for a cancelled chain: status %d: %s", late.StatusCode, raw)
	}
}

func TestEndingASessionReleasesTheEngine(t *testing.T) {
	e := newFakeEngine(t, true)
	n := newInferenceNode(t, e, infer.PriorityNone)

	resp, err := http.DefaultClient.Do(n.request(t, brainToken))
	if err != nil {
		t.Fatal(err)
	}
	<-e.started
	go func() { _, _ = io.ReadAll(resp.Body); resp.Body.Close() }()

	n.mgr.End(sessionName)
	<-e.gone
}

func TestStatsCarryWhatThePlaneSaw(t *testing.T) {
	e := newFakeEngine(t, false)
	n := newInferenceNode(t, e, infer.PriorityNone)
	resp, err := http.DefaultClient.Do(n.request(t, brainToken))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, n.srv.URL+"/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	var body struct {
		Inference *infer.Stats `json:"inference"`
	}
	if err := json.NewDecoder(sresp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Inference == nil || len(body.Inference.Graphs) != 1 || body.Inference.Graphs[0].Completed != 1 {
		t.Fatalf("/v1/stats inference: %+v", body.Inference)
	}

	mreq, _ := http.NewRequest(http.MethodGet, n.srv.URL+"/metrics", nil)
	mreq.Header.Set("Authorization", "Bearer "+operatorToken)
	mresp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatal(err)
	}
	defer mresp.Body.Close()
	metrics, _ := io.ReadAll(mresp.Body)
	for _, want := range []string{
		`aura_inference_requests_total{graph="assistant",outcome="completed"} 1`,
		`aura_inference_tokens_total{graph="assistant",kind="input"} 11`,
		`aura_inference_first_token_seconds_count{graph="assistant"} 1`,
	} {
		if !strings.Contains(string(metrics), want) {
			t.Fatalf("/metrics is missing %q", want)
		}
	}
}
