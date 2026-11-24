package gateway

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

// An Expertise must hear that it is registered before it is handed anything.
//
// Registering made it resolvable, and the acknowledgement followed only after
// the manifest had been persisted. Whatever resolved the capability in between
// reached the socket first. This test is that case made certain rather than
// lucky: something is waiting for each capability and sends to it the instant
// it can be found, which is what a node full of resuming sessions does to an
// Expertise reconnecting after a failover.
func TestAnExpertiseIsToldItIsRegisteredBeforeItIsHandedAnything(t *testing.T) {
	g, h := testGateway(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for round := 0; round < 25; round++ {
		registeredFirst(t, g, srv, round)
	}
}

func registeredFirst(t *testing.T, g *Gateway, srv *httptest.Server, round int) {
	t.Helper()
	capability := fmt.Sprintf("logical.eager%d", round)

	stop := make(chan struct{})
	defer close(stop)
	sent := make(chan bool, 1)
	go func() {
		for {
			select {
			case <-stop:
				sent <- false
				return
			default:
			}
			if live, err := g.Reg.Resolve("", capability); err == nil {
				early, _ := json.Marshal(channel.Envelope{V: "1", ID: channel.NewID(),
					Kind: channel.KindData, Port: "text_in"})
				_ = live.Send(early, channel.QoSReliable)
				sent <- true
				return
			}
		}
	}()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/skill"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage,
		registerFrame(fmt.Sprintf("acme/logical/eager%d", round), capability)); err != nil {
		t.Fatalf("register: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("round %d: nothing came back: %v", round, err)
	}
	var first channel.Envelope
	_ = json.Unmarshal(raw, &first)
	var state struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(first.Payload, &state)
	if first.Kind != channel.KindStatus || state.State != "registered" {
		t.Fatalf("round %d: the first frame was a %q, not the registration — "+
			"a delivery overtook it", round, first.Kind)
	}

	// And the other direction: hearing "registered" means being findable.
	if _, err := g.Reg.Resolve("", capability); err != nil {
		t.Fatalf("round %d: told it was registered, and could not be resolved: %v", round, err)
	}
	if delivered := <-sent; !delivered {
		t.Fatalf("round %d: the waiting sender never found the Expertise", round)
	}
}
