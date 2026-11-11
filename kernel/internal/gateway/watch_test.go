package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

// echoSkill connects a skill that answers every delivery with `n` deltas.
func echoSkill(t *testing.T, srv *httptest.Server, n int) *websocket.Conn {
	t.Helper()
	skill, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/skill"), nil)
	if err != nil {
		t.Fatalf("dial skill: %v", err)
	}
	if err := skill.WriteMessage(websocket.TextMessage,
		registerFrame("acme/logical/echo", "logical.echo")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := skill.ReadMessage(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	go func() {
		for {
			_, raw, err := skill.ReadMessage()
			if err != nil {
				return
			}
			var in channel.Envelope
			if json.Unmarshal(raw, &in) != nil || in.Kind != channel.KindData {
				continue
			}
			for i := 1; i <= n; i++ {
				payload, _ := json.Marshal(map[string]any{"text": "t", "final": i == n})
				out, _ := json.Marshal(channel.Envelope{
					V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: in.ID,
					Session: in.Session, Node: in.Node, Port: "text_out", Seq: uint64(i),
					Idem:   in.Idem + ":out:" + string(rune('a'+i%26)) + channel.NewID(),
					Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
				})
				if skill.WriteMessage(websocket.TextMessage, out) != nil {
					return
				}
			}
		}
	}()
	return skill
}

func readKind(t *testing.T, c *websocket.Conn, kind string) channel.Envelope {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for a %q frame: %v", kind, err)
		}
		var env channel.Envelope
		if json.Unmarshal(raw, &env) == nil && env.Kind == kind {
			return env
		}
	}
}

func liveEchoSession(t *testing.T, n int) (*httptest.Server, *websocket.Conn, string) {
	t.Helper()
	_, h := testGateway(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	skill := echoSkill(t, srv, n)
	t.Cleanup(func() { skill.Close() })

	graph := map[string]any{
		"ir": "1", "graph_id": "live", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "eco", "resolve": "logical.echo"}},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "eco.text_in"},
			{"from": "eco.text_out", "to": "client.text_in"},
		},
	}
	if code, _ := do(t, h, "POST", "/v1/graphs", graph); code != 201 {
		t.Fatalf("register graph: %d", code)
	}
	client, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/v1/stream?graph=live"), nil)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	hello := readKind(t, client, channel.KindStatus)
	var p struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(hello.Payload, &p)
	return srv, client, p.Session
}

func TestWatchSocketReceivesTheSessionsOutput(t *testing.T) {
	srv, client, session := liveEchoSession(t, 5)

	watcher, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/v1/watch?session="+session), nil)
	if err != nil {
		t.Fatalf("dial watcher: %v", err)
	}
	defer watcher.Close()
	hello := readKind(t, watcher, channel.KindStatus)
	var state struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(hello.Payload, &state)
	if state.State != "watching" {
		t.Fatalf("the watcher's first frame says %q", state.State)
	}

	// Something a watcher sends goes nowhere: this would be an input if it
	// were routed, and the skill would answer it.
	_ = watcher.WriteMessage(websocket.TextMessage, []byte(`{"text":"let me in"}`))

	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"text":"go"}`)); err != nil {
		t.Fatal(err)
	}
	var clientIDs, watcherIDs []string
	for i := 0; i < 5; i++ {
		clientIDs = append(clientIDs, readKind(t, client, channel.KindData).ID)
		watcherIDs = append(watcherIDs, readKind(t, watcher, channel.KindData).ID)
	}
	for i := range clientIDs {
		if clientIDs[i] != watcherIDs[i] {
			t.Fatalf("frame %d differs: client %s, watcher %s", i, clientIDs[i], watcherIDs[i])
		}
	}
	// Exactly one reply's worth: the watcher's own frame started nothing.
	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("the session produced more output; a watcher's frame was routed as input")
	}
}

func TestWatchingASessionThatIsNotLiveIsRefusedBeforeTheUpgrade(t *testing.T) {
	_, h := testGateway(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, "/v1/watch?session=nope"), nil)
	if err == nil {
		t.Fatal("a watcher attached to a session that does not exist")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want a readable 404, got %v", resp)
	}
	_, resp, err = websocket.DefaultDialer.Dial(wsURL(srv, "/v1/watch"), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a watch with no session id: err=%v resp=%v", err, resp)
	}
}

func TestTheWatcherIsToldWhenTheSessionEnds(t *testing.T) {
	srv, client, session := liveEchoSession(t, 1)
	watcher, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/v1/watch?session="+session), nil)
	if err != nil {
		t.Fatalf("dial watcher: %v", err)
	}
	defer watcher.Close()
	readKind(t, watcher, channel.KindStatus) // watching

	client.Close() // the session's owner leaves; the session ends

	_ = watcher.SetReadDeadline(time.Now().Add(5 * time.Second))
	sawEnded := false
	for {
		_, raw, err := watcher.ReadMessage()
		if err != nil {
			break // and then the socket closes
		}
		var env channel.Envelope
		var p struct {
			State string `json:"state"`
		}
		if json.Unmarshal(raw, &env) == nil && json.Unmarshal(env.Payload, &p) == nil && p.State == "ended" {
			sawEnded = true
		}
	}
	if !sawEnded {
		t.Fatal("the watcher's socket closed without saying the session ended")
	}
}

func TestWatchRequiresTheOperatorToken(t *testing.T) {
	h := (&Auth{Token: "s3cret"}).Authenticate(okHandler())
	if code := request(t, h, http.MethodGet, "/v1/watch?session=x", nil); code != http.StatusUnauthorized {
		t.Errorf("/v1/watch without a token returned %d", code)
	}
	scoped := Principal{Scope: ScopeSkill, Capability: "logical.echo"}
	if scoped.Allows(http.MethodGet, "/v1/watch") {
		t.Error("a capability-scoped credential may watch sessions; only the operator should")
	}
	if !isWebSocketPath("/v1/watch") {
		t.Error("/v1/watch is not treated as a WebSocket path, so its Origin is never checked")
	}
}
