// Package mcpsrv projects connected skills as MCP tools.
//
// Standard projection pattern: it ships in the binary for convenience but
// talks to the kernel exclusively as a CLIENT (HTTP + WS on the public
// API), gaining no kernel privileges. Transport: MCP Streamable HTTP with
// single JSON responses (no SSE), served at POST /mcp on the kernel port.
//
//	claude mcp add --transport http aura http://localhost:9080/mcp
package mcpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/approvals"
	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
)

const protocolVersion = "2025-06-18"

type Server struct {
	BaseURL string // http://localhost:<port>
	Version string
	// Token authenticates this projection against its own node. The MCP
	// server holds no kernel privileges — it is a client of the public API
	// like any other — so once the node requires a token, it needs one too.
	Token string
	// Approvals is where a human-approval gate goes to find a human. An MCP
	// client is a program: when the executor gates a `motor.*` call there is
	// nobody on this socket to ask, so the question is parked in the node's
	// approval queue and answered from the UI, `aura approve`, or
	// POST /v1/approvals/{id}.
	//
	// Nil keeps the original behaviour — an automatic denial with an
	// explanation — which is the right fallback: a node with no way to ask
	// must not proceed as though someone said yes.
	Approvals *approvals.Registry
	// NodeID is what a signed approval is bound to (C4 v1.3). Empty on a node
	// that never requires signatures; an operator answering an MCP-originated
	// gate needs it to sign the same statement the executor will verify.
	NodeID string
	Log    *slog.Logger
}

// awaitApproval parks a gate for a human and blocks until it is answered.
//
// Returns the decision and, when one was given, the operator's signed
// statement — which this package deliberately does not inspect. Verifying it is
// the executor's job and only the executor's: a border that decided for itself
// whether a signature was good would be a second implementation of the check,
// and the one place both must agree is the place that seals.
func (s *Server) awaitApproval(req channel.Envelope, tool string, arguments map[string]any) (bool, *ledger.Approval) {
	if s.Approvals == nil {
		return false, nil
	}
	var body struct {
		Question string `json:"question"`
		// Held is the id of the delivery the executor is holding — what a
		// signed approval must be bound to, carried through so an operator
		// answering out of band signs the same delivery the gate is waiting on.
		Held string `json:"held"`
	}
	_ = json.Unmarshal(req.Payload, &body)
	if body.Question == "" {
		body.Question = "Approve this call?"
	}
	args, _ := json.Marshal(arguments)

	id, decision, release := s.Approvals.Open(approvals.Pending{
		Question:  body.Question,
		Origin:    "mcp",
		Tool:      tool,
		Session:   req.Session,
		Arguments: args,
		Node:      s.NodeID,
		Envelope:  body.Held,
	})
	defer release()

	if s.Log != nil {
		s.Log.Info("mcp: waiting for human approval", "id", id, "tool", tool)
	}
	answer := <-decision
	if answer.Approve {
		return true, answer.Approval
	}
	return false, nil
}

// deniedBecause is the sentence the model sees when a call is refused. The two
// reasons stay distinct on purpose: "a person declined this" and "there was
// nobody to ask" call for different responses from whoever reads the
// transcript afterwards.
func deniedBecause(id string) string {
	return fmt.Sprintf(
		"denied: a human declined this call, or it went unanswered (approval %s). "+
			"Pending approvals are listed at GET /v1/approvals.", id)
}

// authorize attaches the node token. Header form rather than query form: this
// is not a browser, and a credential in a URL ends up in logs.
func (s *Server) authorize(h http.Header) {
	if s.Token != "" {
		h.Set("Authorization", "Bearer "+s.Token)
	}
}

// get performs an authenticated GET against the node.
func (s *Server) get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	s.authorize(req.Header)
	return http.DefaultClient.Do(req)
}

// postGraph registers the generated relay graph, reporting a refusal rather
// than swallowing it.
func (s *Server) postGraph(graph []byte) error {
	req, err := http.NewRequest(http.MethodPost, s.BaseURL+"/v1/graphs", bytes.NewReader(graph))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	s.authorize(req.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("register tool graph: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("the node refused the tool graph (%d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// ── JSON-RPC plumbing ────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// GET opens a stream for *server-initiated* messages, which this
			// server has none of: every message it sends answers something the
			// client asked. Streamable HTTP permits 405 for exactly that case.
			// Tool output still streams — over the POST that requested it, just
			// below.
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			s.reply(w, nil, nil, &rpcError{-32700, "parse error"})
			return
		}
		// Notifications carry no id and expect no body.
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// A tools/call from a client that accepts an event stream is answered
		// as one, so a skill's tokens reach the agent as they are produced.
		//
		// This is not a small detail for this runtime in particular: the whole
		// argument of the kernel is that the connection is the unit of work and
		// output streams rather than batching. Serving its own tools over
		// request/response made the border contradict the thesis behind it.
		if req.Method == "tools/call" && acceptsSSE(r) {
			s.streamCall(w, r, req)
			return
		}
		result, rpcErr := s.dispatch(req)
		s.reply(w, req.ID, result, rpcErr)
	}
}

func acceptsSSE(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// streamCall answers one tools/call over SSE.
//
// Intermediate text is reported as `notifications/progress`, which is MCP's
// standard channel for "still working, here is some of it" — and only when the
// client supplied a progressToken, because a notification quoting a token the
// client never issued is one it is entitled to discard. The final JSON-RPC
// response closes the stream either way, so a client that asked for SSE without
// a token still gets a correct, if unstreamed, answer.
func (s *Server) streamCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		result, rpcErr := s.dispatch(req)
		s.reply(w, req.ID, result, rpcErr)
		return
	}
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.reply(w, req.ID, nil, &rpcError{-32602, "invalid params"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// One writer, one goroutine: chunks arrive from the skill socket's reader
	// and the final result from this one, and interleaved writes would corrupt
	// the framing.
	var mu sync.Mutex
	event := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	var progress float64
	emit := func(chunk string) {
		if len(params.Meta.ProgressToken) == 0 || chunk == "" {
			return
		}
		progress++
		event(map[string]any{
			"jsonrpc": "2.0", "method": "notifications/progress",
			"params": map[string]any{
				"progressToken": params.Meta.ProgressToken,
				"progress":      progress,
				"message":       chunk,
			},
		})
	}

	// The request context ends the wait if the agent hangs up mid-call.
	result, rpcErr := s.callToolStreaming(r.Context(), params.Name, params.Arguments, emit)
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	event(resp)
}

func (s *Server) reply(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		resp["error"] = e
	} else {
		resp["result"] = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) dispatch(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "aura", "version": s.Version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools, err := s.listTools()
		if err != nil {
			return nil, &rpcError{-32603, err.Error()}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		return s.callTool(params.Name, params.Arguments)
	default:
		return nil, &rpcError{-32601, "method not found: " + req.Method}
	}
}

// ── skills → tools ───────────────────────────────────────────────

type skillInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Capability  string `json:"capability"`
	Type        string `json:"type"`
	Ports       struct {
		Ingress []struct {
			Name   string `json:"name"`
			Schema string `json:"schema"`
		} `json:"ingress"`
		Egress []struct {
			Name   string `json:"name"`
			Schema string `json:"schema"`
		} `json:"egress"`
	} `json:"ports"`
}

func (s *Server) skills() ([]skillInfo, error) {
	resp, err := s.get("/v1/skills")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("the node rejected this MCP server's token")
	}
	var out []skillInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// toolName sanitizes a capability into MCP's ^[a-zA-Z0-9_-]{1,64}$.
func toolName(capability string) string {
	return strings.NewReplacer(".", "_", "/", "_").Replace(capability)
}

// inputSchema maps our std schemas to a JSON Schema for the tool.
func inputSchema(portSchema string) map[string]any {
	switch {
	case strings.HasPrefix(portSchema, "std/text@"):
		return map[string]any{"type": "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"}}
	case strings.HasPrefix(portSchema, "std/api-request@"):
		return map[string]any{"type": "object", "properties": map[string]any{
			"params": map[string]any{"type": "object"},
			"query":  map[string]any{"type": "object"},
			"body":   map[string]any{}}}
	case strings.HasPrefix(portSchema, "std/document@"):
		return map[string]any{"type": "object", "properties": map[string]any{
			"mime":      map[string]any{"type": "string"},
			"bytes_b64": map[string]any{"type": "string"}},
			"required": []string{"bytes_b64"}}
	default:
		return map[string]any{"type": "object",
			"description": "payload conforming to " + portSchema}
	}
}

func (s *Server) listTools() ([]map[string]any, error) {
	skills, err := s.skills()
	if err != nil {
		return nil, err
	}
	tools := make([]map[string]any, 0, len(skills))
	for _, sk := range skills {
		if len(sk.Ports.Ingress) == 0 {
			continue
		}
		tools = append(tools, map[string]any{
			"name":        toolName(sk.Capability),
			"description": fmt.Sprintf("%s (aura skill %s, input %s)", sk.Description, sk.ID, sk.Ports.Ingress[0].Schema),
			"inputSchema": inputSchema(sk.Ports.Ingress[0].Schema),
		})
	}
	return tools, nil
}

// ── tools/call: run the skill through the standard client path ───

// callTool is the buffered form: everything the skill produced, once it is
// done. Used for a client that did not ask for a stream.
func (s *Server) callTool(name string, arguments map[string]any) (any, *rpcError) {
	return s.callToolStreaming(context.Background(), name, arguments, nil)
}

// callToolStreaming runs the skill through the standard client path, reporting
// each text chunk to emit as it arrives. emit may be nil.
func (s *Server) callToolStreaming(ctx context.Context, name string, arguments map[string]any,
	emit func(string)) (any, *rpcError) {

	if emit == nil {
		emit = func(string) {}
	}
	skills, err := s.skills()
	if err != nil {
		return nil, &rpcError{-32603, err.Error()}
	}
	var target *skillInfo
	for i := range skills {
		if toolName(skills[i].Capability) == name {
			target = &skills[i]
			break
		}
	}
	if target == nil || len(target.Ports.Ingress) == 0 {
		return nil, &rpcError{-32602, "unknown tool: " + name}
	}
	ingress := target.Ports.Ingress[0]

	// Ensure the ephemeral graph exists (idempotent). Policy parity with the
	// planner: edges into motor.* skills carry a human-approval
	// gate — over MCP the gate is refused, so acting always needs a human.
	graphID := "mcp-" + name
	inEdge := map[string]any{"from": "client.mcp_out", "to": "s." + ingress.Name}
	if target.Type == "motor" {
		inEdge["gate"] = "human-approval"
	}
	edges := []map[string]any{inEdge}
	for _, egress := range target.Ports.Egress {
		edges = append(edges, map[string]any{"from": "s." + egress.Name, "to": "client.text_in"})
	}
	graph, _ := json.Marshal(map[string]any{
		"ir": "1", "graph_id": graphID,
		"origin": map[string]string{"kind": "declared"},
		"nodes":  []map[string]any{{"ref": "s", "resolve": target.Capability}},
		"edges":  edges,
	})
	// A rejected graph used to be discarded here, so the failure surfaced later
	// as an unexplained timeout on the stream instead of as the refusal it was.
	if err := s.postGraph(graph); err != nil {
		return nil, &rpcError{-32603, err.Error()}
	}

	// Client session over the public WS — the same path any client uses.
	wsURL := strings.Replace(s.BaseURL, "http", "ws", 1) + "/v1/stream?graph=" + graphID
	wsHeader := http.Header{}
	s.authorize(wsHeader)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, wsHeader)
	if err != nil {
		return nil, &rpcError{-32603, err.Error()}
	}
	defer conn.Close()

	// An agent that hangs up mid-call should not leave this session reading
	// until its deadline: ReadJSON has no context form, so cancellation closes
	// the socket underneath it. The kernel then suppresses the abandoned chain
	// on its own — that guarantee is the executor's, not this projection's.
	ctxDone := make(chan struct{})
	defer close(ctxDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-ctxDone:
		}
	}()
	deadline := time.Now().Add(120 * time.Second)
	_ = conn.SetReadDeadline(deadline)

	var hello channel.Envelope
	if err := conn.ReadJSON(&hello); err != nil {
		return nil, &rpcError{-32603, err.Error()}
	}
	if hello.Kind == channel.KindError {
		return toolError(string(hello.Payload)), nil
	}
	// The session id the kernel assigned, so the effects this call seals can be
	// read back and returned as evidence with the result.
	var ready struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(hello.Payload, &ready)
	session := ready.Session
	if session == "" {
		session = hello.Session
	}

	payload, _ := json.Marshal(arguments)
	env := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Node: "client", Port: "mcp_out", Seq: 1,
		Idem:   "mcp:" + channel.NewID(),
		Schema: ingress.Schema, Kind: channel.KindData, Payload: payload,
	}
	if err := conn.WriteJSON(env); err != nil {
		return nil, &rpcError{-32603, err.Error()}
	}

	// Collect: streamed text joins until final; a single structured reply
	// returns as pretty JSON; a short grace period catches stragglers.
	var text strings.Builder
	var structured json.RawMessage
	gotData := false
	for time.Now().Before(deadline) {
		if gotData {
			_ = conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		}
		var reply channel.Envelope
		if err := conn.ReadJSON(&reply); err != nil {
			break // grace expired or closed — return what we have
		}
		switch reply.Kind {
		case channel.KindData:
			gotData = true
			var body struct {
				Text  *string `json:"text"`
				Final bool    `json:"final"`
			}
			_ = json.Unmarshal(reply.Payload, &body)
			if body.Text != nil {
				text.WriteString(*body.Text)
				emit(*body.Text)
				if body.Final {
					// Same evidence on the early exit as on the ordinary one: a
					// skill that marks its reply final must not be a way to get
					// an effect back without its receipt.
					return withEffects(toolText(text.String()), s.effectsFor(session)), nil
				}
			} else {
				structured = reply.Payload
			}
		case channel.KindDone:
			goto done
		case channel.KindError:
			var body struct {
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(reply.Payload, &body)
			return toolError(body.Detail), nil
		case channel.KindConfirmRequest:
			if s.Approvals == nil {
				return toolError("this action needs human approval and this node has no " +
					"approval queue — run it from the aura UI, or `aura do`"), nil
			}
			approved, signed := s.awaitApproval(reply, name, arguments)
			// The signature rides back on the same confirm_response the verdict
			// does, so the executor verifies and seals it on the one code path
			// that handles every gate, whatever surface answered it.
			decision, _ := json.Marshal(struct {
				Approve  bool             `json:"approve"`
				Approval *ledger.Approval `json:"approval,omitempty"`
			}{approved, signed})
			if err := conn.WriteJSON(channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: reply.ID,
				Kind: channel.KindConfirmResponse, Payload: decision,
			}); err != nil {
				return nil, &rpcError{-32603, err.Error()}
			}
			if !approved {
				return toolError(deniedBecause(reply.ID)), nil
			}
			// Approval can take as long as a person takes. The read budget is
			// restarted from *now* so the wait is not charged against the time
			// the tool itself gets to answer.
			deadline = time.Now().Add(120 * time.Second)
			_ = conn.SetReadDeadline(deadline)
		}
	}
done:
	// Read back what this call actually did before answering. An agent that
	// learns only what a tool *said* has no way to show, later, what it *did*
	// or on whose authority — and by the time anyone asks, the call is a line
	// in a transcript with nothing attached to it.
	effects := s.effectsFor(session)

	if structured != nil {
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, structured, "", "  ")
		return withEffects(toolText(pretty.String()), effects), nil
	}
	if text.Len() > 0 {
		return withEffects(toolText(text.String()), effects), nil
	}
	return toolError("no reply from the skill"), nil
}

func toolText(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

// EffectsMetaKey is where a tool result carries evidence of what it did.
//
// MCP tells an agent which tools exist and what they returned. It has nothing
// to say about what a call *did to the world*, or on whose authority — so an
// agent that calls `create_invoice` gets back some text and no way to
// demonstrate afterwards that the call was authorized, by whom, or that the
// record of it has not been edited since.
//
// `_meta` is the extension point MCP provides for exactly this, and the key is
// reverse-DNS namespaced per its convention. What travels here is a *receipt* —
// the hash of a sealed ledger entry — never the entry itself and never a
// payload. A receipt is small, is safe to log, and is checkable by anyone with
// `aura receipt --verify`, which needs no database, no node and no network.
//
// See spec/proposals/mcp-effect-receipts.md for the argument that this belongs
// in MCP rather than in one implementation's private namespace.
const EffectsMetaKey = "org.deepaxiom/effects"

// EffectReceipt is one sealed effect a tool call produced, as an MCP client
// sees it.
type EffectReceipt struct {
	Receipt    string `json:"receipt"`
	Capability string `json:"capability"`
	Decision   string `json:"decision"`
	Outcome    string `json:"outcome"`
	// Approver names the human who answered the gate, when one signed
	// (C4 v1.3). Absent on an effect the policy allowed outright, which is a
	// meaningful difference and not a gap.
	Approver string `json:"approver,omitempty"`
}

// withEffects attaches receipts to a tool result.
//
// Attached rather than returned separately so that an agent, a transcript and a
// log all carry the evidence together with the thing it is evidence *of*. A
// receipt filed anywhere else is one that has to be correlated back later,
// which in practice means never.
func withEffects(result map[string]any, effects []EffectReceipt) map[string]any {
	if len(effects) == 0 {
		return result
	}
	meta, _ := result["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[EffectsMetaKey] = effects
	result["_meta"] = meta
	return result
}

// effectsFor reads back what this session actually sealed.
//
// Read from the ledger rather than collected from the stream, deliberately: the
// receipt on an envelope is attached to the delivery going *into* the skill, and
// this server is a client watching what comes back out. Asking the ledger means
// the evidence an agent receives is the same evidence `aura verify` will check
// — one source, no second path that could disagree with it.
func (s *Server) effectsFor(session string) []EffectReceipt {
	if session == "" {
		return nil
	}
	resp, err := s.get("/v1/sessions/" + session + "/ledger")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Entries []ledger.Entry `json:"entries"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil {
		return nil
	}
	out := make([]EffectReceipt, 0, len(body.Entries))
	for _, e := range body.Entries {
		r := EffectReceipt{
			Receipt: e.Hash(), Capability: e.Capability,
			Decision: e.Decision, Outcome: e.Outcome,
		}
		if e.Approver != nil {
			r.Approver = e.Approver.Operator
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toolError(detail string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": detail}},
		"isError": true,
	}
}
