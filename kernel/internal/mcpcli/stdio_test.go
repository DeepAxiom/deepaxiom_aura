package mcpcli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// stdio is how nearly every MCP server in the wild actually ships, so it is the
// transport that most needs exercising against a real subprocess rather than a
// mock. The subprocess here is this test binary re-invoked with a marker in the
// environment — portable, and no fixture script to keep in sync with the code.

const fakeServerEnv = "MCPCLI_FAKE_STDIO_SERVER"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeServerEnv) {
	case "":
		os.Exit(m.Run())
	case "crash":
		os.Exit(1) // exits before answering anything
	default:
		fakeStdioServer(os.Getenv(fakeServerEnv))
	}
}

// fakeStdioServer speaks newline-delimited JSON-RPC on stdin/stdout.
func fakeStdioServer(mode string) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 4<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// Noise on stderr: a server that logs must not deadlock the client, which
	// it would if nothing drained the pipe.
	fmt.Fprintln(os.Stderr, "fake mcp server starting")

	for in.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == 0 {
			continue // a notification: no reply, by design
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": ProtocolVersion,
				"serverInfo":      map[string]any{"name": "stdio-fake", "version": "0.1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "echo", "description": "echoes"},
				map[string]any{"name": "peek", "annotations": map[string]any{"readOnlyHint": true}},
			}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if mode == "slow" {
				time.Sleep(30 * time.Second)
			}
			result = map[string]any{"content": []any{map[string]any{
				"type": "text", "text": fmt.Sprintf("%s:%v", p.Name, p.Arguments["v"])}}}
		}
		line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		_, _ = out.Write(append(line, '\n'))
		_ = out.Flush()
	}
}

func dialFake(t *testing.T, mode string) *Client {
	t.Helper()
	c, err := DialStdio(context.Background(), os.Args[0], nil,
		[]string{fakeServerEnv + "=" + mode})
	if err != nil {
		t.Fatalf("DialStdio: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestStdioInitializeListAndCall(t *testing.T) {
	c := dialFake(t, "ok")
	if c.ServerName != "stdio-fake" {
		t.Errorf("server name = %q", c.ServerName)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].ReadOnly() {
		t.Error("an unannotated tool must not read as read-only")
	}
	if !tools[1].ReadOnly() {
		t.Error("the annotated tool should read as read-only")
	}
	res, err := c.CallTool(context.Background(), "echo", map[string]any{"v": 42})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := res.Text(); got != "echo:42" {
		t.Errorf("text = %q", got)
	}
}

// Concurrent calls must not cross-deliver: the transport demultiplexes by id,
// and getting that wrong would hand one tool's answer to another's caller.
func TestStdioDemultiplexesConcurrentCalls(t *testing.T) {
	c := dialFake(t, "ok")
	const n = 24
	type got struct {
		want, have string
	}
	results := make(chan got, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			res, err := c.CallTool(context.Background(), "echo", map[string]any{"v": i})
			if err != nil {
				results <- got{fmt.Sprintf("echo:%d", i), "error: " + err.Error()}
				return
			}
			results <- got{fmt.Sprintf("echo:%d", i), res.Text()}
		}(i)
	}
	for i := 0; i < n; i++ {
		select {
		case g := <-results:
			if g.want != g.have {
				t.Errorf("answer mismatch: want %q, got %q", g.want, g.have)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a concurrent call never returned")
		}
	}
}

// A server that dies must release everyone waiting on it rather than leaving
// each caller to time out separately.
func TestStdioServerExitIsReported(t *testing.T) {
	_, err := DialStdio(context.Background(), os.Args[0], nil,
		[]string{fakeServerEnv + "=crash"})
	if err == nil {
		t.Fatal("dialling a server that exits immediately must fail")
	}
}

func TestStdioCallAfterCloseIsRefused(t *testing.T) {
	c := dialFake(t, "ok")
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := c.CallTool(context.Background(), "echo", nil); err == nil {
		t.Error("a call on a closed client must fail, not hang")
	}
}

func TestStdioRespectsContextCancellation(t *testing.T) {
	c := dialFake(t, "slow")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.CallTool(ctx, "echo", nil); err == nil {
		t.Fatal("a cancelled call must return an error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %v; it should be immediate", elapsed)
	}
}

func TestStdioDialingSomethingThatIsNotAServer(t *testing.T) {
	_, err := DialStdio(context.Background(), "definitely-not-a-real-binary-xyz", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("want a start failure, got %v", err)
	}
}
