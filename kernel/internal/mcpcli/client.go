// Package mcpcli is an MCP *client*: it speaks to servers someone else runs.
//
// The node already had the other half — internal/mcpsrv projects this node's
// skills as tools for Claude Code, Cursor and friends. That direction makes
// aura a provider. This one makes it a consumer, and the consumer direction is
// what `aura guard` is built on: an agent's tool calls arrive here, pass the
// kernel's policy checkpoint, and only then reach the server that actually
// does the work.
//
// Two transports, because those are the two that exist in the wild:
//
//   - stdio — the server is a subprocess and JSON-RPC travels newline-delimited
//     over its stdin/stdout. This is how nearly every MCP server ships today.
//   - http — JSON-RPC over POST, MCP's Streamable HTTP. An `Accept` of
//     text/event-stream is offered because a compliant server may answer either
//     a single JSON body or an SSE stream to the same POST, and a client that
//     cannot read the second form breaks against servers that stream.
//
// What this deliberately does NOT do: interpret, filter or rewrite anything a
// server returns. Judgement about whether a call may happen belongs to the node
// policy and the executor, one layer up. A client that also decided things
// would be a second checkpoint, and the entire argument of this runtime is that
// there is exactly one.
package mcpcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the MCP revision this client negotiates. It matches
// internal/mcpsrv so a node fronting another node speaks one dialect.
const ProtocolVersion = "2025-06-18"

// maxBody bounds a single response. An MCP server is untrusted input: it may be
// a subprocess someone installed from a registry, and an unbounded read is a
// memory exhaustion primitive handed to whoever wrote it.
const maxBody = 8 << 20

// Tool is one tool as a server describes it.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations *Annotations    `json:"annotations,omitempty"`
}

// Annotations are the server's *hints* about what a tool does (MCP tool
// annotations). The naming in the spec is deliberate and worth preserving here:
// they are hints, not guarantees. A server asserts `readOnlyHint` the same way
// a skill asserts an inference attestation — the claim is useful, and it is
// still only a claim. Guard treats a missing or false `readOnlyHint` as "this
// acts on the world", which is the safe direction to be wrong in.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ReadOnly reports whether the server claims this tool only reads.
//
// Absent annotations mean "unknown", and unknown resolves to false — a tool
// that has not said it is read-only is treated as one that writes. That default
// is the whole safety posture of `aura guard` in one line: an unannotated
// server gets its calls gated rather than waved through.
func (t Tool) ReadOnly() bool {
	if t.Annotations == nil || t.Annotations.ReadOnlyHint == nil {
		return false
	}
	if t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
		return false // contradictory hints resolve against the server
	}
	return *t.Annotations.ReadOnlyHint
}

// CallResult is a tool's answer.
type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Text flattens the content blocks a human or a model will read.
func (r CallResult) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// ── JSON-RPC ────────────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a server-side refusal. It is surfaced rather than flattened into
// a generic failure because "the server said no, and here is its reason" and
// "the transport broke" call for different responses from the operator.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// transport is the wire underneath. Both implementations are request/response
// at this level; SSE framing, when a server chooses it, is unwrapped in the
// http transport so the caller never sees the difference.
type transport interface {
	roundTrip(ctx context.Context, body []byte) ([]byte, error)
	notify(ctx context.Context, body []byte) error
	Close() error
}

// Client is a connection to one MCP server.
type Client struct {
	tr   transport
	mu   sync.Mutex
	next int64

	// ServerName and ServerVersion are what the server called itself during
	// initialize. Recorded because guard puts them in the skill manifest, which
	// is what an operator reads when deciding whether to trust a capability.
	ServerName    string
	ServerVersion string
}

// DialStdio starts `command args...` and speaks JSON-RPC over its pipes.
//
// env is appended to the current environment rather than replacing it. That is
// the wrong default for a security boundary and the right one here: this
// package is a client, and the isolation decision belongs to --sandbox, which
// is the thing that actually makes the environment an allowlist. Two mechanisms
// that both half-scrub the environment produce a gap between them.
func DialStdio(ctx context.Context, command string, args []string, env []string) (*Client, error) {
	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// stderr is drained to nowhere rather than left unread: a full pipe blocks
	// the child forever, and an MCP server that logs to stderr is normal.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	t := &stdioTransport{
		cmd: cmd, in: stdin,
		out:     bufio.NewReaderSize(stdout, 1<<16),
		pending: map[int64]chan rpcResponse{},
	}
	go t.read()
	c := &Client{tr: t}
	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// DialHTTP connects to an MCP server over Streamable HTTP.
func DialHTTP(ctx context.Context, url string, header http.Header) (*Client, error) {
	t := &httpTransport{url: url, header: header.Clone(), hc: &http.Client{Timeout: 120 * time.Second}}
	c := &Client{tr: t}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() error { return c.tr.Close() }

func (c *Client) id() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return c.next
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: c.id(), Method: method, Params: params})
	if err != nil {
		return err
	}
	raw, err := c.tr.roundTrip(ctx, body)
	if err != nil {
		return err
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("%s: malformed response: %w", method, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil {
		return nil
	}
	if len(resp.Result) == 0 {
		return fmt.Errorf("%s: empty result", method)
	}
	return json.Unmarshal(resp.Result, out)
}

func (c *Client) initialize(ctx context.Context) error {
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "aura", "version": "guard"},
	}, &res)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	c.ServerName, c.ServerVersion = res.ServerInfo.Name, res.ServerInfo.Version

	// The spec requires this notification after a successful initialize. A
	// server that gates tools/list on it answers an empty catalog without it,
	// which reads as "this server has no tools" — a confusing way to fail.
	n, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	_ = c.tr.notify(ctx, n)
	return nil
}

// ListTools returns the server's catalog, following pagination to the end.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" || len(res.Tools) == 0 {
			return all, nil
		}
		cursor = res.NextCursor
		if len(all) > 4096 {
			return all, nil // a server paginating forever does not get to spin us
		}
	}
}

// CallTool invokes one tool. A tool that fails *within* MCP semantics comes
// back as a CallResult with IsError set, not as a Go error: the distinction is
// between "the tool ran and reported a problem" (the model should see it) and
// "the call never happened" (the operator should).
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res CallResult
	if err := c.call(ctx, "tools/call", map[string]any{
		"name": name, "arguments": args,
	}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ── stdio transport ─────────────────────────────────────────────────

type stdioTransport struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader

	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	closed  bool
}

// read demultiplexes the server's output. One goroutine owns the reader, so
// concurrent callers never interleave reads of a half-written line.
func (t *stdioTransport) read() {
	for {
		line, err := t.out.ReadBytes('\n')
		if len(line) > 0 {
			var resp rpcResponse
			if json.Unmarshal(bytes.TrimSpace(line), &resp) == nil && resp.ID != 0 {
				t.mu.Lock()
				ch, ok := t.pending[resp.ID]
				delete(t.pending, resp.ID)
				t.mu.Unlock()
				if ok {
					ch <- resp
				}
			}
		}
		if err != nil {
			t.fail()
			return
		}
	}
}

// fail releases every caller blocked on a server that has stopped answering,
// rather than leaving them on a context deadline apiece.
func (t *stdioTransport) fail() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for id, ch := range t.pending {
		close(ch)
		delete(t.pending, id)
	}
}

func (t *stdioTransport) roundTrip(ctx context.Context, body []byte) ([]byte, error) {
	var req struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &req)

	ch := make(chan rpcResponse, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("mcp server has exited")
	}
	t.pending[req.ID] = ch
	t.mu.Unlock()

	if err := t.write(body); err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, err
	}
	select {
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp server closed the connection")
		}
		return json.Marshal(resp)
	}
}

func (t *stdioTransport) notify(_ context.Context, body []byte) error { return t.write(body) }

func (t *stdioTransport) write(body []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fmt.Errorf("mcp server has exited")
	}
	if _, err := t.in.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("write to mcp server: %w", err)
	}
	return nil
}

func (t *stdioTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	_ = t.in.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_, _ = t.cmd.Process.Wait()
	}
	return nil
}

// ── http transport ──────────────────────────────────────────────────

type httpTransport struct {
	url    string
	header http.Header
	hc     *http.Client
}

func (t *httpTransport) roundTrip(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range t.header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	// Both forms are accepted because Streamable HTTP lets the server pick.
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mcp server returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return sseData(raw)
	}
	return raw, nil
}

// notifyTimeout bounds a fire-and-forget message. A notification has no reply
// to wait for — the spec has the server answer 202 and move on — so a server
// that holds the connection open instead must not be able to stall the client
// for its full request timeout. Without this bound, one badly-behaved server
// turned `initialize` into a two-minute hang.
// A var rather than a const so the test for this can run in milliseconds
// instead of waiting the real bound out.
var notifyTimeout = 10 * time.Second

func (t *httpTransport) notify(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range t.header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := t.hc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.Body.Close()
}

func (t *httpTransport) Close() error { return nil }

// sseData pulls the last JSON-RPC message out of an SSE body.
//
// The last one, not the first: a server may emit progress notifications ahead
// of the result, and those carry no id. Taking the final data frame that parses
// as a response with an id is what survives that.
func sseData(raw []byte) ([]byte, error) {
	var last []byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), maxBody)
	var cur bytes.Buffer
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		b := append([]byte(nil), cur.Bytes()...)
		var probe rpcResponse
		if json.Unmarshal(b, &probe) == nil && probe.ID != 0 {
			last = b
		}
		cur.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			cur.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	if last == nil {
		return nil, fmt.Errorf("no JSON-RPC response in the event stream")
	}
	return last, nil
}
