package mcpsrv

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/approvals"
	"aura/kernel/internal/channel"
)

// The two behaviours that let `aura guard` work at all:
//
//   - a tools/call streams, so an agent sees a skill's output as it is produced
//     rather than after it is finished. A runtime whose whole argument is that
//     the connection is the unit of work should not serve its own tools over
//     request/response.
//   - a human-approval gate can actually be answered, instead of resolving to
//     an automatic denial. Until it could, this border could front read-only
//     tools and nothing else.

// streamingNode stubs the kernel: the HTTP surface mcpsrv reads, plus a client
// WebSocket that behaves like a session.
type streamingNode struct {
	srv *httptest.Server
	// gate makes the session raise a human-approval request before answering.
	gate bool
	// chunks are streamed as separate non-final data envelopes.
	chunks []string
}

func newStreamingNode(t *testing.T, skills []map[string]any, gate bool, chunks []string) *streamingNode {
	t.Helper()
	n := &streamingNode{gate: gate, chunks: chunks}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/skills", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(skills)
	})
	mux.HandleFunc("/v1/graphs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(channel.Envelope{
			V: channel.ProtocolMajor, ID: channel.NewID(), Kind: channel.KindStatus})

		var in channel.Envelope
		if err := conn.ReadJSON(&in); err != nil {
			return
		}
		if n.gate {
			q, _ := json.Marshal(map[string]any{"question": "Approve delivery?"})
			reqID := channel.NewID()
			_ = conn.WriteJSON(channel.Envelope{
				V: channel.ProtocolMajor, ID: reqID, CauseID: in.ID,
				Kind: channel.KindConfirmRequest, Payload: q})

			var answer channel.Envelope
			if err := conn.ReadJSON(&answer); err != nil {
				return
			}
			var body struct {
				Approve bool `json:"approve"`
			}
			_ = json.Unmarshal(answer.Payload, &body)
			if !body.Approve {
				e, _ := json.Marshal(map[string]string{"detail": "denied"})
				_ = conn.WriteJSON(channel.Envelope{
					V: channel.ProtocolMajor, ID: channel.NewID(),
					Kind: channel.KindError, Payload: e})
				return
			}
		}
		for i, c := range n.chunks {
			p, _ := json.Marshal(map[string]any{
				"text": c, "final": i == len(n.chunks)-1})
			_ = conn.WriteJSON(channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: in.ID,
				Kind: channel.KindData, Schema: "std/text@1", Payload: p})
			time.Sleep(10 * time.Millisecond)
		}
	})
	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

func textSkill() []map[string]any {
	return []map[string]any{{
		"id": "acme/cognitive/writer", "name": "writer", "description": "writes",
		"capability": "cognitive.write", "type": "cognitive",
		"ports": map[string]any{
			"ingress": []map[string]any{{"name": "text_in", "schema": "std/text@1"}},
			"egress":  []map[string]any{{"name": "text_out", "schema": "std/text@1"}},
		},
	}}
}

func motorSkill() []map[string]any {
	return []map[string]any{{
		"id": "acme/motor/writer", "name": "erp", "description": "writes to the ERP",
		"capability": "motor.erp.write", "type": "motor",
		"ports": map[string]any{
			"ingress": []map[string]any{{"name": "call", "schema": "std/text@1"}},
			"egress":  []map[string]any{{"name": "result", "schema": "std/text@1"}},
		},
	}}
}

// callSSE posts a tools/call asking for an event stream and returns the frames.
func callSSE(t *testing.T, s *Server, tool string, withToken bool) []map[string]any {
	t.Helper()
	params := map[string]any{"name": tool, "arguments": map[string]any{"text": "hi"}}
	if withToken {
		params["_meta"] = map[string]any{"progressToken": 1}
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	s.Handler()(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q; the server did not stream", ct)
	}
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestToolCallStreamsProgress(t *testing.T) {
	n := newStreamingNode(t, textSkill(), false, []string{"Hola", " mundo", "!"})
	s := &Server{BaseURL: n.srv.URL, Version: "test", Log: testLogger()}

	frames := callSSE(t, s, "cognitive_write", true)
	if len(frames) < 2 {
		t.Fatalf("want progress frames plus a result, got %d: %v", len(frames), frames)
	}
	var progress, results int
	var streamed strings.Builder
	for _, f := range frames {
		if f["method"] == "notifications/progress" {
			progress++
			p, _ := f["params"].(map[string]any)
			if msg, ok := p["message"].(string); ok {
				streamed.WriteString(msg)
			}
			continue
		}
		if f["result"] != nil {
			results++
		}
	}
	if progress != 3 {
		t.Errorf("progress notifications = %d, want one per chunk (3)", progress)
	}
	if got := streamed.String(); got != "Hola mundo!" {
		t.Errorf("streamed text = %q", got)
	}
	if results != 1 {
		t.Errorf("result frames = %d, want exactly 1", results)
	}
	last := frames[len(frames)-1]
	if last["result"] == nil {
		t.Error("the stream must end with the JSON-RPC response")
	}
}

// A client that asks for a stream without a progress token still gets a
// correct answer — just not an incremental one.
func TestStreamWithoutProgressTokenStillAnswers(t *testing.T) {
	n := newStreamingNode(t, textSkill(), false, []string{"a", "b"})
	s := &Server{BaseURL: n.srv.URL, Version: "test", Log: testLogger()}

	frames := callSSE(t, s, "cognitive_write", false)
	if len(frames) != 1 || frames[0]["result"] == nil {
		t.Fatalf("want exactly one result frame, got %v", frames)
	}
}

// A client that did not ask for a stream keeps the buffered JSON body.
func TestNonStreamingClientStillGetsJSON(t *testing.T) {
	n := newStreamingNode(t, textSkill(), false, []string{"a", "b"})
	s := &Server{BaseURL: n.srv.URL, Version: "test", Log: testLogger()}

	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "cognitive_write", "arguments": map[string]any{"text": "hi"}}})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	s.Handler()(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q; a non-streaming client must get JSON", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body: %v", err)
	}
	if resp["result"] == nil {
		t.Errorf("no result: %v", resp)
	}
}

// ── the gate ────────────────────────────────────────────────────────

func TestGateIsAnsweredByAHumanAndTheCallProceeds(t *testing.T) {
	n := newStreamingNode(t, motorSkill(), true, []string{"written"})
	appr := approvals.New()
	s := &Server{BaseURL: n.srv.URL, Version: "test", Log: testLogger(), Approvals: appr}

	// Approve out-of-band, the way an operator or the UI would.
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if list := appr.List(); len(list) == 1 {
				_ = appr.Resolve(list[0].ID, approvals.Answer{Approve: true})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	result, rpcErr := s.callTool("motor_erp_write", map[string]any{"text": "x"})
	if rpcErr != nil {
		t.Fatalf("rpc error: %v", rpcErr)
	}
	m, _ := result.(map[string]any)
	if m["isError"] == true {
		t.Fatalf("an approved call must not report an error: %v", m)
	}
	if !strings.Contains(contentText(m), "written") {
		t.Errorf("the approved call did not reach the skill: %v", m)
	}
}

func TestDeniedGateIsExplainedToTheModel(t *testing.T) {
	n := newStreamingNode(t, motorSkill(), true, []string{"written"})
	appr := approvals.New()
	s := &Server{BaseURL: n.srv.URL, Version: "test", Log: testLogger(), Approvals: appr}

	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if list := appr.List(); len(list) == 1 {
				_ = appr.Resolve(list[0].ID, approvals.Answer{Approve: false})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	result, rpcErr := s.callTool("motor_erp_write", map[string]any{"text": "x"})
	if rpcErr != nil {
		t.Fatalf("rpc error: %v", rpcErr)
	}
	m, _ := result.(map[string]any)
	if m["isError"] != true {
		t.Fatalf("a denial must be reported as an error: %v", m)
	}
	if !strings.Contains(contentText(m), "denied") {
		t.Errorf("the model should be told it was denied: %v", contentText(m))
	}
}

// With no queue configured the old behaviour has to hold: refuse, and say why.
// Proceeding as though someone said yes would be the one unacceptable failure.
func TestGateWithNoApprovalQueueStillRefuses(t *testing.T) {
	n := newStreamingNode(t, motorSkill(), true, []string{"written"})
	s := &Server{BaseURL: n.srv.URL, Version: "test", Log: testLogger()} // no Approvals

	result, rpcErr := s.callTool("motor_erp_write", map[string]any{"text": "x"})
	if rpcErr != nil {
		t.Fatalf("rpc error: %v", rpcErr)
	}
	m, _ := result.(map[string]any)
	if m["isError"] != true {
		t.Fatalf("want a refusal, got %v", m)
	}
	if !strings.Contains(contentText(m), "no approval queue") {
		t.Errorf("the refusal should name the cause: %v", contentText(m))
	}
}

// An unanswered gate expires into a denial rather than holding the agent open.
func TestUnansweredGateExpiresIntoADenial(t *testing.T) {
	n := newStreamingNode(t, motorSkill(), true, []string{"written"})
	s := &Server{BaseURL: n.srv.URL, Version: "test", Log: testLogger(),
		Approvals: approvals.WithTTL(100 * time.Millisecond)}

	done := make(chan map[string]any, 1)
	go func() {
		result, _ := s.callTool("motor_erp_write", map[string]any{"text": "x"})
		m, _ := result.(map[string]any)
		done <- m
	}()
	select {
	case m := <-done:
		if m["isError"] != true {
			t.Errorf("an expired gate must deny, got %v", m)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("an unanswered gate hung the caller")
	}
}

func contentText(m map[string]any) string {
	content, _ := m["content"].([]map[string]any)
	if content == nil {
		if raw, ok := m["content"].([]any); ok {
			var b strings.Builder
			for _, c := range raw {
				if cm, ok := c.(map[string]any); ok {
					if s, ok := cm["text"].(string); ok {
						b.WriteString(s)
					}
				}
			}
			return b.String()
		}
	}
	var b strings.Builder
	for _, c := range content {
		if s, ok := c["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}
