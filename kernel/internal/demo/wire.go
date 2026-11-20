package demo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

type envelope = channel.Envelope

type textPayload struct {
	Text  string `json:"text"`
	Final bool   `json:"final"`
}

// textOf reads a std/text@1 payload. Anything else reads as empty.
func textOf(env envelope) string {
	var p textPayload
	_ = json.Unmarshal(env.Payload, &p)
	return p.Text
}

// stateOf reads the state a status or error envelope reports, and its detail.
func stateOf(env envelope) (state, detail string) {
	var p struct {
		State  string `json:"state"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(env.Payload, &p)
	return p.State, p.Detail
}

// eventOf reads the event a status envelope reports, and how late it says the
// delivery was, where it says.
func eventOf(env envelope) (event string, lateMS int64) {
	var p struct {
		Event  string `json:"event"`
		LateMS int64  `json:"late_ms"`
	}
	_ = json.Unmarshal(env.Payload, &p)
	return p.Event, p.LateMS
}

// dial opens a WebSocket to the node with the operator's token.
func (c *cluster) dial(path string) (*websocket.Conn, error) { return c.dialAs(c.token, path) }

// dialAs is dial with a credential other than the operator's.
func (c *cluster) dialAs(token, path string) (*websocket.Conn, error) {
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	conn, _, err := d.Dial(c.wsBase()+path, h)
	return conn, err
}

// ── an Expertise ────────────────────────────────────────────────────

// expertise is one Expertise connected to the node: it registers a manifest,
// is handed deliveries, and emits. When its connection drops it dials again
// until it is back, which is what lets it follow a node to its standby.
type expertise struct {
	c *cluster
	// token is this Expertise's own credential: good for registering as its
	// one capability and for the few routes an Expertise needs.
	token    string
	manifest map[string]any
	// handle runs once per data delivery, on its own goroutine.
	handle func(x *expertise, env envelope)
	// onCancel, when set, is told the cause a cancel names.
	onCancel func(cause string)

	mu   sync.Mutex // guards conn and every write to it
	conn *websocket.Conn

	seq       atomic.Uint64
	up        atomic.Bool
	connects  atomic.Int64
	lastUp    atomic.Int64 // unix nanos of the last registration
	delivered atomic.Int64

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

func manifest(id, capability, kind string, ingress, egress []string) map[string]any {
	ports := func(names []string) []map[string]any {
		out := make([]map[string]any, 0, len(names))
		for _, n := range names {
			out = append(out, map[string]any{"name": n, "schema": "std/text@1"})
		}
		return out
	}
	return map[string]any{
		"id": id, "version": "1.0.0", "protocol": "1",
		"name": id, "description": "part of `aura demo`",
		"capability": capability, "type": kind, "format": "source",
		"ports": map[string]any{"ingress": ports(ingress), "egress": ports(egress)},
	}
}

func (c *cluster) expertise(token string, m map[string]any, handle func(*expertise, envelope)) *expertise {
	x := &expertise{c: c, token: token, manifest: m, handle: handle,
		stop: make(chan struct{}), done: make(chan struct{})}
	go x.run()
	return x
}

func (x *expertise) stopped() bool {
	select {
	case <-x.stop:
		return true
	default:
		return false
	}
}

func (x *expertise) run() {
	defer close(x.done)
	payload, _ := json.Marshal(x.manifest)
	for !x.stopped() {
		conn, err := x.c.dialAs(x.token, "/ws/expertise")
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		_ = conn.WriteJSON(envelope{V: "1", ID: channel.NewID(), Kind: channel.KindRegister, Payload: payload})
		x.mu.Lock()
		x.conn = conn
		x.mu.Unlock()

		// The answer to the registration is recognised by what it says, not
		// by being first: whatever else arrives is a delivery and is handled
		// as one.
		registered, refused := false, false
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		for !refused {
			var env envelope
			if err := conn.ReadJSON(&env); err != nil {
				break
			}
			if !registered {
				switch state, _ := stateOf(env); {
				case env.Kind == channel.KindStatus && state == "registered":
					registered = true
					_ = conn.SetReadDeadline(time.Time{})
					x.connects.Add(1)
					x.lastUp.Store(time.Now().UnixNano())
					x.up.Store(true)
					continue
				case env.Kind == channel.KindError:
					refused = true
					continue
				}
			}
			switch env.Kind {
			case channel.KindData:
				x.delivered.Add(1)
				go x.handle(x, env)
			case channel.KindCancel:
				if x.onCancel != nil {
					x.onCancel(env.CauseID)
				}
			}
		}
		x.up.Store(false)
		x.mu.Lock()
		x.conn = nil
		x.mu.Unlock()
		conn.Close()
		if refused {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

var errDown = errors.New("the Expertise is not connected")

// emit sends one std/text@1 output, caused by the delivery it answers.
func (x *expertise) emit(cause envelope, port, text string, final bool) error {
	payload, _ := json.Marshal(textPayload{Text: text, Final: final})
	env := envelope{
		V: "1", ID: channel.NewID(), CauseID: cause.ID,
		Session: cause.Session, Node: cause.Node, Port: port,
		Seq: x.seq.Add(1), Idem: channel.NewID(),
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.conn == nil {
		return errDown
	}
	_ = x.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return x.conn.WriteJSON(env)
}

func (x *expertise) close() {
	x.stopOnce.Do(func() { close(x.stop) })
	x.mu.Lock()
	if x.conn != nil {
		x.conn.Close()
	}
	x.mu.Unlock()
	select {
	case <-x.done:
	case <-time.After(2 * time.Second):
	}
}

// ── a client session ────────────────────────────────────────────────

// client is one application session: a socket on /v1/stream.
type client struct {
	graph, session string
	conn           *websocket.Conn
	wmu            sync.Mutex
	seq            uint64
}

// refused is a session the node declined to open, with what it said.
type refused struct{ detail string }

func (r refused) Error() string { return "the node refused the session: " + r.detail }

// open starts or resumes a session. A node that will not run the graph says so
// in an error envelope where the greeting would be; that comes back as refused.
func (c *cluster) open(graph, session string) (*client, error) {
	path := "/v1/stream?graph=" + url.QueryEscape(graph) + "&session=" + url.QueryEscape(session)
	conn, err := c.dial(path)
	if err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hello envelope
	if err := conn.ReadJSON(&hello); err != nil {
		conn.Close()
		return nil, err
	}
	if state, detail := stateOf(hello); hello.Kind == channel.KindError || state != "ready" {
		conn.Close()
		return nil, refused{detail: detail}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return &client{graph: graph, session: session, conn: conn}, nil
}

// say sends one message as a full envelope, so its id is known to the sender:
// a cancel names it, and a reconnect can never collide with it.
func (cl *client) say(text string) (string, error) {
	id := channel.NewID()
	return id, cl.sayAs(id, text)
}

// sayAs sends a message under an id the caller chose. Sending the same id
// again is what a client does when it never learned whether the first one
// arrived: it is the same request, and says so.
func (cl *client) sayAs(id, text string) error {
	payload, _ := json.Marshal(textPayload{Text: text, Final: true})
	cl.wmu.Lock()
	defer cl.wmu.Unlock()
	cl.seq++
	_ = cl.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return cl.conn.WriteJSON(envelope{
		V: "1", ID: id, Kind: channel.KindData, Port: "text_out", Seq: cl.seq,
		Idem: id, Schema: "std/text@1", Payload: payload,
	})
}

func (cl *client) cancel(id string) error {
	cl.wmu.Lock()
	defer cl.wmu.Unlock()
	_ = cl.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return cl.conn.WriteJSON(envelope{V: "1", ID: channel.NewID(), Kind: channel.KindCancel, CauseID: id})
}

// read returns the next envelope, waiting at most limit.
func (cl *client) read(limit time.Duration) (envelope, error) {
	var env envelope
	_ = cl.conn.SetReadDeadline(time.Now().Add(limit))
	err := cl.conn.ReadJSON(&env)
	return env, err
}

// await reads until an envelope satisfies want, or limit passes.
func (cl *client) await(limit time.Duration, want func(envelope) bool) (envelope, error) {
	end := time.Now().Add(limit)
	for {
		left := time.Until(end)
		if left <= 0 {
			return envelope{}, fmt.Errorf("nothing matching arrived within %s", limit)
		}
		env, err := cl.read(left)
		if err != nil {
			return envelope{}, err
		}
		if want(env) {
			return env, nil
		}
	}
}

func (cl *client) close() {
	if cl != nil && cl.conn != nil {
		cl.conn.Close()
	}
}

func isData(env envelope) bool { return env.Kind == channel.KindData }
