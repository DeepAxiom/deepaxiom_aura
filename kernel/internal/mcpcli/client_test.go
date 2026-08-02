package mcpcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A fake MCP server over HTTP, answering the three methods a client uses.
func fakeServer(t *testing.T, opts fakeOpts) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == 0 { // a notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": ProtocolVersion,
				"serverInfo":      map[string]any{"name": "fake", "version": "1.2.3"},
			}
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if opts.paginate && p.Cursor == "" {
				result = map[string]any{
					"tools":      []any{map[string]any{"name": "page1"}},
					"nextCursor": "c2",
				}
			} else if opts.paginate {
				result = map[string]any{"tools": []any{map[string]any{"name": "page2"}}}
			} else {
				result = map[string]any{"tools": opts.tools}
			}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			result = map[string]any{
				"content": []any{map[string]any{"type": "text",
					"text": fmt.Sprintf("ran %s with %v", p.Name, p.Arguments["k"])}},
			}
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if opts.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			// A progress notification first, so the client has to pick the
			// right frame rather than the first one.
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "data: %s\n\n", resp)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fakeOpts struct {
	tools    []any
	sse      bool
	paginate bool
}

func TestInitializeRecordsServerIdentity(t *testing.T) {
	srv := fakeServer(t, fakeOpts{})
	c, err := DialHTTP(context.Background(), srv.URL, http.Header{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if c.ServerName != "fake" || c.ServerVersion != "1.2.3" {
		t.Errorf("server identity = %q/%q; want fake/1.2.3", c.ServerName, c.ServerVersion)
	}
}

func TestListAndCallOverHTTP(t *testing.T) {
	srv := fakeServer(t, fakeOpts{tools: []any{
		map[string]any{"name": "read_file", "description": "reads"},
	}})
	c, err := DialHTTP(context.Background(), srv.URL, http.Header{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "read_file" {
		t.Fatalf("tools = %+v err=%v", tools, err)
	}
	res, err := c.CallTool(context.Background(), "read_file", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := res.Text(); got != "ran read_file with v" {
		t.Errorf("text = %q", got)
	}
}

// A server may answer the same POST with an event stream; the client must read
// the response out of it and ignore the notifications ahead of it.
func TestSSEResponseIsUnwrapped(t *testing.T) {
	srv := fakeServer(t, fakeOpts{sse: true, tools: []any{map[string]any{"name": "t"}}})
	c, err := DialHTTP(context.Background(), srv.URL, http.Header{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools = %+v err = %v", tools, err)
	}
}

func TestListToolsFollowsPagination(t *testing.T) {
	srv := fakeServer(t, fakeOpts{paginate: true})
	c, _ := DialHTTP(context.Background(), srv.URL, http.Header{})
	defer c.Close()
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "page1" || tools[1].Name != "page2" {
		t.Errorf("pagination not followed: %+v", tools)
	}
}

// The safety-critical unit: what counts as read-only. Getting this wrong in the
// permissive direction is what would let a server disarm `aura guard`.
func TestReadOnlyDefaultsToFalse(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		tool Tool
		want bool
	}{
		{"no annotations at all", Tool{}, false},
		{"annotations present but silent", Tool{Annotations: &Annotations{}}, false},
		{"explicitly not read-only", Tool{Annotations: &Annotations{ReadOnlyHint: &no}}, false},
		{"explicitly read-only", Tool{Annotations: &Annotations{ReadOnlyHint: &yes}}, true},
		{"read-only but destructive wins", Tool{Annotations: &Annotations{
			ReadOnlyHint: &yes, DestructiveHint: &yes}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tool.ReadOnly(); got != tc.want {
				t.Errorf("ReadOnly() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServerErrorSurfacesAsRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32000, "message": "upstream is angry"},
		})
	}))
	defer srv.Close()

	_, err := DialHTTP(context.Background(), srv.URL, http.Header{})
	if err == nil || !strings.Contains(err.Error(), "upstream is angry") {
		t.Fatalf("want the server's own reason, got %v", err)
	}
}

func TestContextCancellationReleasesCaller(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"serverInfo": map[string]any{"name": "slow"}}})
			return
		}
		if req.ID == 0 { // notifications are answered promptly, as a server should
			w.WriteHeader(http.StatusAccepted)
			return
		}
		<-block // a real call never answers
	}))
	defer srv.Close()
	defer close(block)

	c, err := DialHTTP(context.Background(), srv.URL, http.Header{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "x", nil); err == nil {
		t.Fatal("a cancelled call must return an error, not hang")
	}
}

// A notification has no reply. A server that holds the connection open on one
// must not stall the client for its whole request timeout — this used to turn
// a single unresponsive server into a two-minute hang at startup.
func TestNotificationDoesNotStallInitialize(t *testing.T) {
	old := notifyTimeout
	notifyTimeout = 100 * time.Millisecond
	defer func() { notifyTimeout = old }()

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"serverInfo": map[string]any{"name": "rude"}}})
			return
		}
		<-block // never answers the notification either
	}))
	// Order matters: Close waits for outstanding handlers, so the blocked one
	// has to be released first.
	defer srv.Close()
	defer close(block)

	done := make(chan error, 1)
	go func() {
		c, err := DialHTTP(context.Background(), srv.URL, http.Header{})
		if c != nil {
			_ = c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("initialize should still succeed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initialize hung on an unanswered notification")
	}
}

func TestSSEDataPicksTheLastResponse(t *testing.T) {
	raw := []byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\n\n")
	got, err := sseData(raw)
	if err != nil {
		t.Fatalf("sseData: %v", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(got, &resp); err != nil || resp.ID != 7 {
		t.Errorf("picked the wrong frame: %s", got)
	}
}

func TestSSEWithNoResponseIsAnError(t *testing.T) {
	if _, err := sseData([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"x\"}\n\n")); err == nil {
		t.Error("a stream with no response must not look like success")
	}
}
