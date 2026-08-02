package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// shortLiveness shrinks the ping/pong timers for the duration of a test.
func shortLiveness(t *testing.T, ping, pong time.Duration) {
	t.Helper()
	oldPing, oldPong := pingPeriod, pongWait
	pingPeriod, pongWait = ping, pong
	t.Cleanup(func() { pingPeriod, pongWait = oldPing, oldPong })
}

func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

func registerFrame(id, capability string) []byte {
	b, _ := json.Marshal(map[string]any{
		"v": "1", "id": "01TESTREGISTER0000000000AA", "kind": "register",
		"payload": map[string]any{
			"id": id, "version": "1.0.0", "protocol": "1",
			"name": id, "description": "liveness test skill",
			"capability": capability, "type": "logical", "format": "source",
			"ports": map[string]any{
				"ingress": []map[string]string{{"name": "text_in", "schema": "std/text@1"}},
			},
		},
	})
	return b
}

// A skill that stops responding entirely — the socket stays open, nothing is
// read or written — must be detected and cleaned up. Before the read deadline
// existed, this connection leaked its registry entry and its admission
// reservation for the lifetime of the process.
func TestZombieSkillConnectionIsReaped(t *testing.T) {
	shortLiveness(t, 50*time.Millisecond, 300*time.Millisecond)

	g, h := testGateway(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// A raw TCP connection speaking the websocket handshake by hand, so we
	// can go silent afterwards: a gorilla client would auto-pong and stay
	// alive, which is exactly what we must not do here.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/skill"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage,
		registerFrame("acme/logical/zombie", "logical.zombie")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil { // the register ack
		t.Fatalf("ack: %v", err)
	}

	if _, err := g.Reg.Resolve("", "logical.zombie"); err != nil {
		t.Fatalf("skill should be registered right after the ack: %v", err)
	}

	// Silence the peer: swallow pings instead of ponging them.
	conn.SetPingHandler(func(string) error { return nil })
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := g.Reg.Resolve("", "logical.zombie"); err != nil {
			return // unregistered — the connection was reaped
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a skill that stopped responding was never unregistered; the read deadline did not fire")
}

// The mirror image: a peer that behaves must NOT be reaped. Without this, a
// too-aggressive deadline would look like a fix while silently killing every
// idle-but-healthy skill.
func TestHealthySkillConnectionSurvivesManyPingCycles(t *testing.T) {
	shortLiveness(t, 20*time.Millisecond, 200*time.Millisecond)

	g, h := testGateway(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/skill"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage,
		registerFrame("acme/logical/healthy", "logical.healthy")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// gorilla's default ping handler pongs for us as long as we keep reading.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Well past pongWait, and many ping periods.
	time.Sleep(600 * time.Millisecond)

	if _, err := g.Reg.Resolve("", "logical.healthy"); err != nil {
		t.Fatalf("a peer that ponged was reaped anyway: %v", err)
	}
}

// Idle client sessions get the same treatment: without it, one abandoned
// browser tab holds a session and its writer goroutine forever.
func TestZombieClientSessionIsReaped(t *testing.T) {
	shortLiveness(t, 50*time.Millisecond, 300*time.Millisecond)

	_, h := testGateway(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// A graph with no skills still instantiates? No — it needs a resolvable
	// node, so register one first over a second connection.
	skill, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/skill"), nil)
	if err != nil {
		t.Fatalf("dial skill: %v", err)
	}
	defer skill.Close()
	if err := skill.WriteMessage(websocket.TextMessage,
		registerFrame("acme/logical/echo", "logical.echo")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := skill.ReadMessage(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	go func() {
		for {
			if _, _, err := skill.ReadMessage(); err != nil {
				return
			}
		}
	}()

	graph := map[string]any{
		"ir": "1", "graph_id": "live", "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "eco", "resolve": "logical.echo"}},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "eco.text_in"},
		},
	}
	if code, _ := do(t, h, "POST", "/v1/graphs", graph); code != 201 {
		t.Fatalf("register graph: %d", code)
	}

	client, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/v1/stream?graph=live"), nil)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	defer client.Close()
	if _, _, err := client.ReadMessage(); err != nil { // the ready status
		t.Fatalf("ready: %v", err)
	}

	// Go silent: never pong, never read.
	client.SetPingHandler(func(string) error { return nil })

	// The server should close the connection; our next read then errors.
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("want the server to drop a client that stopped responding")
	}
}

// The server must actually emit pings — the read deadline alone would just
// disconnect healthy idle peers. This also exercises the single-writer
// discipline: pings share the writer goroutine with envelope delivery.
func TestServerSendsPings(t *testing.T) {
	shortLiveness(t, 20*time.Millisecond, 2*time.Second)

	_, h := testGateway(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/skill"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	pings := make(chan struct{}, 32)
	conn.SetPingHandler(func(appData string) error {
		select {
		case pings <- struct{}{}:
		default:
		}
		return conn.WriteControl(websocket.PongMessage, []byte(appData),
			time.Now().Add(time.Second))
	})

	if err := conn.WriteMessage(websocket.TextMessage,
		registerFrame("acme/logical/pinged", "logical.pinged")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-pings:
		case <-time.After(2 * time.Second):
			t.Fatalf("only received %d ping(s); the server is not pinging", i)
		}
	}
}
