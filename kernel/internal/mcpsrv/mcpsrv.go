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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

const protocolVersion = "2025-06-18"

type Server struct {
	BaseURL string // http://localhost:<port>
	Version string
	// Token authenticates this projection against its own node. The MCP
	// server holds no kernel privileges — it is a client of the public API
	// like any other — so once the node requires a token, it needs one too.
	Token string
	Log   *slog.Logger
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
		result, rpcErr := s.dispatch(req)
		s.reply(w, req.ID, result, rpcErr)
	}
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

func (s *Server) callTool(name string, arguments map[string]any) (any, *rpcError) {
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
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, wsHeader)
	if err != nil {
		return nil, &rpcError{-32603, err.Error()}
	}
	defer conn.Close()
	deadline := time.Now().Add(120 * time.Second)
	_ = conn.SetReadDeadline(deadline)

	var hello channel.Envelope
	if err := conn.ReadJSON(&hello); err != nil {
		return nil, &rpcError{-32603, err.Error()}
	}
	if hello.Kind == channel.KindError {
		return toolError(string(hello.Payload)), nil
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
				if body.Final {
					return toolText(text.String()), nil
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
			// Human gates cannot be answered over MCP: deny and explain.
			deny, _ := json.Marshal(map[string]bool{"approve": false})
			_ = conn.WriteJSON(channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: reply.ID,
				Kind: channel.KindConfirmResponse, Payload: deny,
			})
			return toolError("this action requires human approval — run it from the aura UI or `aura do`"), nil
		}
	}
done:
	if structured != nil {
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, structured, "", "  ")
		return toolText(pretty.String()), nil
	}
	if text.Len() > 0 {
		return toolText(text.String()), nil
	}
	return toolError("no reply from the skill"), nil
}

func toolText(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

func toolError(detail string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": detail}},
		"isError": true,
	}
}
