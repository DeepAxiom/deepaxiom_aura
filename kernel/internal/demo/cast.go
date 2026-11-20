package demo

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The Expertise the demo runs, and the graphs that wire them.
//
//	relay     answers every message with itself, as fast as it can. The load
//	          that measures the node's routing passes through it.
//	brain     asks the model — through the node, never directly — and streams
//	          the tokens back. For an operation it then issues an order.
//	planner   issues an order after a delay it is told, which is how an order
//	          comes to be late.
//	actuator  the only one that acts on the world. It is handed a sealed
//	          effect, spends that effect's receipt at the broker for the
//	          service key, and calls the service.
//	payments  provides a capability the policy forbids. It is connected and
//	          able; it must never be handed anything.

const (
	graphRelay = "demo-relay"
	graphOps   = "demo-ops"
	graphTalk  = "demo-talk"
	graphLate  = "demo-late"
	graphExfil = "demo-exfil"

	capRelay    = "logical.demo.relay"
	capBrain    = "cognitive.demo.brain"
	capPlanner  = "logical.demo.planner"
	capActuator = "motor.demo.dispatch"
	capPayments = "motor.payments.send"

	// lateDeadlineMS is how long an order on the late graph stays valid.
	lateDeadlineMS = 250
)

func graphs() []map[string]any {
	g := func(id string, nodes []map[string]any, edges []map[string]any) map[string]any {
		return map[string]any{"ir": "1", "graph_id": id,
			"origin": map[string]string{"kind": "declared"}, "nodes": nodes, "edges": edges}
	}
	n := func(ref, capability string) map[string]any {
		return map[string]any{"ref": ref, "resolve": capability}
	}
	e := func(from, to string) map[string]any { return map[string]any{"from": from, "to": to} }
	return []map[string]any{
		g(graphRelay, []map[string]any{n("relay", capRelay)}, []map[string]any{
			e("client.text_out", "relay.text_in"),
			e("relay.text_out", "client.text_in"),
		}),
		g(graphOps, []map[string]any{n("brain", capBrain), n("act", capActuator)}, []map[string]any{
			{"from": "client.text_out", "to": "brain.text_in", "priority": 5},
			e("brain.text_out", "client.text_in"),
			e("brain.order_out", "act.order_in"),
			e("act.text_out", "client.text_in"),
		}),
		g(graphTalk, []map[string]any{n("brain", capBrain)}, []map[string]any{
			e("client.text_out", "brain.text_in"),
			e("brain.text_out", "client.text_in"),
		}),
		g(graphLate, []map[string]any{n("plan", capPlanner), n("act", capActuator)}, []map[string]any{
			{"from": "client.text_out", "to": "plan.text_in", "deadline_ms": lateDeadlineMS},
			e("plan.order_out", "act.order_in"),
			e("act.text_out", "client.text_in"),
		}),
		// The graph an attacker registers: it wires the forbidden capability and
		// waives the gate on it. It is a well-formed document, so it registers.
		g(graphExfil, []map[string]any{n("pay", capPayments)}, []map[string]any{
			{"from": "client.text_out", "to": "pay.order_in", "gate": "none"},
		}),
	}
}

// cast is every Expertise the demo connects.
type cast struct {
	c        *cluster
	relays   []*expertise
	brains   []*expertise
	planner  *expertise
	actuator *expertise
	payments *expertise

	svc   *service
	model string
	// streamer carries the brain's calls to the node. No timeout: a generation
	// is as long as it is, and the node is what cuts it.
	streamer *http.Client

	redeemed   atomic.Int64 // receipts spent for the key
	dispatched atomic.Int64 // orders the actuator carried out
	actFailed  atomic.Int64
	actErr     atomic.Value // string: the last thing that went wrong for the actuator
	inferErr   atomic.Value // string: the last thing that went wrong for the brain

	handedToPayments atomic.Int64

	mu       sync.Mutex
	actuated map[string]bool // order ids the actuator was handed
}

func (c *cluster) cast(svc *service, model string, tokens map[string]string, relays, brains int) *cast {
	k := &cast{c: c, svc: svc, model: model, actuated: map[string]bool{},
		streamer: &http.Client{Transport: &http.Transport{
			MaxIdleConns: 256, MaxIdleConnsPerHost: 256, IdleConnTimeout: 30 * time.Second}}}
	for i := 0; i < relays; i++ {
		k.relays = append(k.relays, c.expertise(tokens[capRelay],
			manifest("demo/logical/relay", capRelay, "logical",
				[]string{"text_in"}, []string{"text_out"}), k.relay))
	}
	for i := 0; i < brains; i++ {
		k.brains = append(k.brains, c.expertise(tokens[capBrain],
			manifest("demo/cognitive/brain", capBrain, "cognitive",
				[]string{"text_in"}, []string{"text_out", "order_out"}), k.brain))
	}
	k.planner = c.expertise(tokens[capPlanner], manifest("demo/logical/planner", capPlanner, "logical",
		[]string{"text_in"}, []string{"order_out"}), k.plan)
	k.actuator = c.expertise(tokens[capActuator], manifest("demo/motor/dispatch", capActuator, "motor",
		[]string{"order_in"}, []string{"text_out"}), k.act)
	k.payments = c.expertise(tokens[capPayments], manifest("demo/motor/payments", capPayments, "motor",
		[]string{"order_in"}, []string{"text_out"}),
		func(*expertise, envelope) { k.handedToPayments.Add(1) })
	return k
}

func (k *cast) all() []*expertise {
	out := append([]*expertise{}, k.relays...)
	out = append(out, k.brains...)
	return append(out, k.planner, k.actuator, k.payments)
}

// connected reports whether every Expertise is registered right now.
func (k *cast) connected() bool {
	for _, x := range k.all() {
		if !x.up.Load() {
			return false
		}
	}
	return true
}

func (k *cast) waitConnected(limit time.Duration) bool {
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if k.connected() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func (k *cast) close() {
	for _, x := range k.all() {
		x.close()
	}
}

// relay echoes. `burst <n> <bytes>` asks for n numbered messages of that size
// instead, which is how a session comes to have a great deal waiting for it.
func (k *cast) relay(x *expertise, env envelope) {
	text := textOf(env)
	if !strings.HasPrefix(text, "burst ") {
		_ = x.emit(env, "text_out", text, true)
		return
	}
	var n, size int
	if _, err := fmt.Sscanf(text, "burst %d %d", &n, &size); err != nil || n <= 0 {
		return
	}
	pad := strings.Repeat("x", size)
	for i := 1; i <= n; i++ {
		if x.emit(env, "text_out", strconv.Itoa(i)+":"+pad, i == n) != nil {
			return
		}
		// Paced like a stream: a few hundred messages a second, which is
		// faster than a reader that has stopped but is not one giant write.
		time.Sleep(2 * time.Millisecond)
	}
}

// brain asks the model through the node and streams what comes back.
//
//	op <order>           a short generation, then an order for the actuator
//	talk <tokens> <tag>  a long generation and nothing else
func (k *cast) brain(x *expertise, env envelope) {
	text := textOf(env)
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return
	}
	tokens := 12
	if fields[0] == "talk" {
		tokens, _ = strconv.Atoi(fields[1])
	}
	body, _ := json.Marshal(map[string]any{
		"model": k.model, "stream": true, "max_tokens": tokens,
		"messages": []map[string]string{{"role": "user", "content": text}},
	})
	req, err := http.NewRequest(http.MethodPost,
		k.c.base()+"/v1/inference/chat/completions", bytes.NewReader(body))
	if err != nil {
		return
	}
	// The call names the delivery it is made for. That is all the node needs
	// to place it: which graph, which priority, which deadline, and whether the
	// chain it belongs to is still wanted.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+x.token)
	req.Header.Set("X-Aura-Session", env.Session)
	req.Header.Set("X-Aura-Delivery", env.ID)
	req.Header.Set("X-Aura-Node", env.Node)
	req.Header.Set("X-Aura-Port", env.Port)
	resp, err := k.streamer.Do(req)
	if err != nil {
		k.inferErr.Store(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		k.inferErr.Store("inference answered " + resp.Status)
		return
	}
	complete := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := line[len("data: "):]
		if bytes.Equal(data, []byte("[DONE]")) {
			complete = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		if tok := chunk.Choices[0].Delta.Content; tok != "" {
			if x.emit(env, "text_out", tok, false) != nil {
				return
			}
		}
	}
	// An order follows only a generation that finished. One the node cut —
	// cancelled, out of time — decided nothing, so nothing is ordered.
	if complete && fields[0] == "op" {
		_ = x.emit(env, "order_out", fields[1], true)
	}
}

// plan issues an order, late or on time: `order <delay-ms> <id>`.
func (k *cast) plan(x *expertise, env envelope) {
	var delay int
	var id string
	if _, err := fmt.Sscanf(textOf(env), "order %d %s", &delay, &id); err != nil {
		return
	}
	if delay > 0 {
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
	_ = x.emit(env, "order_out", id, true)
}

// act carries out one order: receipt to the broker, key to the service.
//
// The key is in this function's memory for the length of one call and is not
// kept. That is the whole design the broker enables — there is no standing
// credential in this process for anything to steal.
func (k *cast) act(x *expertise, env envelope) {
	id := textOf(env)
	k.mu.Lock()
	k.actuated[id] = true
	k.mu.Unlock()

	fail := func(why string) {
		k.actFailed.Add(1)
		k.actErr.Store(why)
	}
	if env.Receipt == "" {
		fail("an effect was delivered with no receipt")
		return
	}
	var grant struct {
		Values map[string]string `json:"values"`
		Error  string            `json:"error"`
	}
	code, err := k.c.callAs(x.token, http.MethodPost, "/v1/secrets/resolve", map[string]any{
		"receipt": env.Receipt, "capability": capActuator, "names": []string{secretName},
	}, &grant)
	if err != nil || code != 200 {
		fail(fmt.Sprintf("the broker answered %d %s %v", code, grant.Error, err))
		return
	}
	k.redeemed.Add(1)

	body, _ := json.Marshal(map[string]string{"id": id})
	req, _ := http.NewRequest(http.MethodPost, k.svc.url()+"/orders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+grant.Values[secretName])
	resp, err := k.c.http.Do(req)
	if err != nil {
		fail("the service did not answer: " + err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		fail("the service answered " + resp.Status)
		return
	}
	k.dispatched.Add(1)
	_ = x.emit(env, "text_out", "done "+id, true)
}

// handed reports whether the actuator was ever handed this order.
func (k *cast) handed(id string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.actuated[id]
}

func loadString(v *atomic.Value) string {
	s, _ := v.Load().(string)
	return s
}
