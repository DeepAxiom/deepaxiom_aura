package demo

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"aura/kernel/internal/channel"
)

// The six beats.
//
// Each one does something to the running system, measures what happened, and
// states what had to be true for the system to have survived it. A check that
// does not hold is reported as failed on the screen, in the same place a
// passing one would be: a demo that could only ever show success would be a
// recording, not a measurement.

// What each beat sets out to do. Fixed here so the screen can say what is about
// to happen before it does.
const (
	slowBurst  = 1200    // messages sent to each session that stopped reading
	slowBytes  = 8 << 10 // the size of each
	lateEach   = 20      // orders issued late, and as many in time
	lateDelay  = 600     // how long the planner takes with a late one, in ms
	cutGens    = 16      // generations cancelled at once
	cutPlanned = 4000    // tokens each of them was going to produce
)

// slowSessions is how many sessions stop reading, out of n.
func slowSessions(n int) int { return max(2, min(6, n/12)) }

// plan is what the screen is told about a beat before it runs.
func plan(id string, sessions int) map[string]any {
	switch id {
	case "slow":
		return map[string]any{"sessions": slowSessions(sessions), "burst": slowBurst, "bytes": slowBytes}
	case "late":
		return map[string]any{"late": lateEach, "on_time": lateEach,
			"delay_ms": lateDelay, "deadline_ms": lateDeadlineMS}
	case "cut":
		return map[string]any{"generations": cutGens}
	}
	return map[string]any{}
}

// ── 1. sessions that stop reading ───────────────────────────────────

// beatSlow takes a handful of sessions, stops reading from them, and has an
// Expertise send each one more than its socket will take. The Expertise serves
// every other session over the same connection, so if the node waited on the
// slow ones, everyone would wait.
//
// The readers stop for about three seconds and then catch up. That is the case
// the node is built for — a consumer that falls behind — and not the one it
// refuses to carry: a delivery that cannot be made for five seconds is dropped
// and its sender told, because holding it forever would be the node choosing
// one session's backlog over its own memory.
func (r *Run) beatSlow(b *beat) {
	const (
		burst = slowBurst
		size  = slowBytes
		stall = 3200 * time.Millisecond
	)
	relay := r.lanes.relay
	k := slowSessions(len(relay))
	var lent []*flow
	for i := 0; i < k && i < len(relay); i++ {
		lent = append(lent, relay[(i*len(relay))/k+len(relay)/(2*k)])
	}
	b.fact("sessions", len(lent))
	b.fact("burst", burst)
	b.fact("bytes", size)

	now := time.Now()
	before := r.lanes.rec.between(now.Add(-3*time.Second), now)
	st0, _ := r.c.stats()

	for _, f := range lent {
		f.lent.Store(true)
		f.state.Store(dotSlow)
		f.kick()
	}
	defer func() {
		for _, f := range lent {
			f.lent.Store(false)
		}
	}()
	time.Sleep(200 * time.Millisecond)

	// The same sessions, resumed, and then not read.
	clients := make([]*client, 0, len(lent))
	defer func() {
		for _, cl := range clients {
			cl.close()
		}
	}()
	for _, f := range lent {
		cl, err := r.c.open(f.graph, f.id)
		if err != nil {
			b.check("took_over", false, "could not resume %s: %v", f.id, err)
			return
		}
		clients = append(clients, cl)
	}

	t0 := time.Now()
	for _, cl := range clients {
		if _, err := cl.say(fmt.Sprintf("burst %d %d", burst, size)); err != nil {
			b.check("took_over", false, "could not ask for the burst: %v", err)
			return
		}
	}
	var peak int64
	for time.Since(t0) < stall {
		if st, ok := r.c.stats(); ok && st.held() > peak {
			peak = st.held()
			b.fact("held_peak", peak)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t1 := time.Now()
	during := r.lanes.rec.between(t0, t1)
	st1, _ := r.c.stats()

	// The readers come back. Everything the node was holding has to arrive,
	// in the order it was sent.
	received := make([]int, len(clients))
	ordered := make([]bool, len(clients))
	var wg sync.WaitGroup
	for i, cl := range clients {
		wg.Add(1)
		go func(i int, cl *client) {
			defer wg.Done()
			ordered[i] = true
			for received[i] < burst {
				env, err := cl.read(20 * time.Second)
				if err != nil {
					return
				}
				if env.Kind != channel.KindData {
					continue
				}
				text := textOf(env)
				colon := strings.IndexByte(text, ':')
				if colon < 0 {
					continue // an answer to something sent before the takeover
				}
				n, _ := strconv.Atoi(text[:colon])
				if n != received[i]+1 {
					ordered[i] = false
				}
				received[i]++
			}
		}(i, cl)
	}
	wg.Wait()
	drain := time.Since(t1)

	total, inOrder := 0, true
	for i := range clients {
		total += received[i]
		inOrder = inOrder && ordered[i]
	}
	waited := st1.waits() - st0.waits()
	others := float64(len(relay)-len(lent)) / float64(len(relay))

	b.fact("held_peak", peak)
	b.fact("producer_waits", waited)
	b.fact("rate_before", round(before.Rate, 0))
	b.fact("rate_during", round(during.Rate, 0))
	b.fact("p50_before", round(before.P50, 2))
	b.fact("p50_during", round(during.P50, 2))
	b.fact("p99_before", round(before.P99, 2))
	b.fact("p99_during", round(during.P99, 2))
	b.fact("received", total)
	b.fact("expected", burst*len(clients))
	b.fact("drain_ms", ms(drain))

	b.check("node_held_the_backlog", peak > 0,
		"the node never had to hold anything, so nothing was at stake")
	b.check("no_producer_waited", waited == 0,
		"%d enqueues found a session's queue full and blocked the Expertise", waited)
	b.check("others_kept_their_pace", during.Rate >= 0.9*before.Rate*others,
		"round trips fell from %.0f/s to %.0f/s", before.Rate, during.Rate)
	b.check("nothing_lost_or_reordered", total == burst*len(clients) && inOrder,
		"%d of %d arrived, in order: %v", total, burst*len(clients), inOrder)
}

// ── 2. a compromised Expertise ──────────────────────────────────────

// beatAgent acts as an Expertise that has been taken over — the one talking to
// the model, which is where an injected instruction lands — and tries every
// way it has to make the service carry out an order. It holds exactly what
// that Expertise holds: its own token.
func (r *Run) beatAgent(b *beat) {
	const tries = 20
	token := r.cast.brains[0].token
	stamp := time.Now().UnixNano() % 1_000_000
	executedBefore := r.svc.executed.Load()
	var orders []string

	// Straight to the service. It has no key, so it sends none, or guesses.
	direct := 0
	for i := 0; i < tries; i++ {
		id := fmt.Sprintf("attack-%d-%d", stamp, i)
		orders = append(orders, id)
		key := ""
		if i%2 == 1 {
			key = newKey()
		}
		if orderDirect(r.c.http, r.svc, id, key) == http.StatusUnauthorized {
			direct++
		}
	}

	// To the broker, for the key: with no receipt, and with invented ones.
	broker := 0
	for i := 0; i < tries; i++ {
		receipt := ""
		if i%2 == 1 {
			raw := make([]byte, 32)
			_, _ = rand.Read(raw)
			receipt = "sha256:" + hex.EncodeToString(raw)
		}
		var out struct {
			Values map[string]string `json:"values"`
		}
		code, _ := r.c.callAs(token, http.MethodPost, "/v1/secrets/resolve", map[string]any{
			"receipt": receipt, "capability": capActuator, "names": []string{secretName},
		}, &out)
		if code == http.StatusForbidden && len(out.Values) == 0 {
			broker++
		}
	}

	// Looking for something to steal: real receipts are in the ledger, and
	// the secrets have names.
	paths := []string{"/v1/ledger", "/v1/secrets", "/v1/sessions", "/v1/graphs", "/v1/stats"}
	reads := 0
	for _, p := range paths {
		if code, _ := r.c.callAs(token, http.MethodGet, p, nil, nil); code == 403 || code == 401 {
			reads++
		}
	}

	// Becoming the Expertise that is allowed to act.
	const disguises = 5
	imposter := 0
	for i := 0; i < disguises; i++ {
		if !r.c.registersAs(token, manifest("demo/motor/dispatch", capActuator, "motor",
			[]string{"order_in"}, []string{"text_out"})) {
			imposter++
		}
	}

	// Wiring the forbidden capability into a graph of its own.
	exfil := graphs()[len(graphs())-1]
	exfil["graph_id"] = fmt.Sprintf("attack-%d", stamp)
	code, _ := r.c.callAs(token, http.MethodPost, "/v1/graphs", exfil, nil)
	graphRefused := code == 403 || code == 401

	// And the case where it did not have to: someone with the operator's
	// token already registered such a graph, gate waived. Now it only has to
	// be run — and this is asked with the operator's token, the strongest
	// position there is.
	denied, why := 0, ""
	for i := 0; i < tries; i++ {
		id := fmt.Sprintf("attack-%d-pay-%d", stamp, i)
		orders = append(orders, id)
		ok, detail := r.c.runForbidden(id)
		if !ok {
			denied++
			why = detail
		}
	}
	time.Sleep(300 * time.Millisecond)

	reached := 0
	for _, id := range orders {
		if r.svc.carriedOut(id) {
			reached++
		}
	}
	handed := r.cast.handedToPayments.Load()

	b.fact("attempts", 2*tries+len(paths)+disguises+1+tries)
	b.fact("direct", direct)
	b.fact("broker", broker)
	b.fact("reads", reads)
	b.fact("disguises", imposter)
	b.fact("denied", denied)
	b.fact("reached", reached)
	b.fact("handed_to_forbidden", handed)
	b.fact("legit_meanwhile", r.svc.executed.Load()-executedBefore)
	if why != "" {
		b.fact("policy_said", why)
	}

	b.check("service_refused_every_direct_call", direct == tries,
		"%d of %d calls without the key were refused", direct, tries)
	b.check("broker_released_nothing", broker == tries,
		"%d of %d requests for the key were refused", broker, tries)
	b.check("could_not_read_the_ledger_or_secrets", reads == len(paths),
		"%d of %d reads were refused", reads, len(paths))
	b.check("could_not_register_as_the_actuator", imposter == disguises,
		"%d of %d registrations were refused", imposter, disguises)
	b.check("could_not_register_a_graph", graphRefused, "the node answered %d", code)
	b.check("policy_denied_the_forbidden_effect", denied == tries,
		"%d of %d runs were refused", denied, tries)
	b.check("nothing_reached_the_service", reached == 0 && handed == 0,
		"%d orders were carried out and %d were handed to the forbidden Expertise", reached, handed)
}

// orderDirect calls the service the way a process with no key can.
func orderDirect(h *http.Client, svc *service, id, key string) int {
	body, _ := json.Marshal(map[string]string{"id": id})
	req, _ := http.NewRequest(http.MethodPost, svc.url()+"/orders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := h.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// registersAs reports whether the node accepted a registration made with this
// token for this manifest.
func (c *cluster) registersAs(token string, m map[string]any) bool {
	conn, err := c.dialAs(token, "/ws/expertise")
	if err != nil {
		return false
	}
	defer conn.Close()
	payload, _ := json.Marshal(m)
	_ = conn.WriteJSON(envelope{V: "1", ID: channel.NewID(), Kind: channel.KindRegister, Payload: payload})
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ack envelope
	if err := conn.ReadJSON(&ack); err != nil {
		return false
	}
	state, _ := stateOf(ack)
	return state == "registered"
}

// runForbidden tries to run the graph that wires the forbidden capability. ok
// is true only if the node let the order through.
func (c *cluster) runForbidden(id string) (ok bool, detail string) {
	cl, err := c.open(graphExfil, "demo-x-"+id)
	var no refused
	if errors.As(err, &no) {
		return false, no.detail
	}
	if err != nil {
		return false, err.Error()
	}
	defer cl.close()
	if _, err := cl.say(id); err != nil {
		return false, err.Error()
	}
	env, err := cl.await(2*time.Second, func(env envelope) bool {
		return env.Kind == channel.KindError || env.Kind == channel.KindData
	})
	if err != nil {
		// Silence: nothing came back, and whether anything went out is
		// settled by what the forbidden Expertise was handed.
		return false, "no answer"
	}
	if env.Kind == channel.KindError {
		_, detail = stateOf(env)
		return false, detail
	}
	return true, ""
}

// ── 3. orders that arrive too late ──────────────────────────────────

// beatLate sends orders through a graph that gives them a quarter of a second
// to be valid. Half are issued at once. Half are issued by a planner that took
// more than twice that long.
//
// What it requires is that every order has exactly one fate: carried out, or
// sealed as expired and never carried out. It does not require that every
// order issued "at once" made its deadline — on a machine slow enough, one may
// not, and then expiring it is the node being right. That case is counted and
// shown rather than called a failure.
func (r *Run) beatLate(b *beat) {
	const (
		each  = lateEach
		delay = lateDelay
	)
	stamp := time.Now().UnixNano() % 1_000_000
	st0, _ := r.c.stats()

	type order struct {
		id, session string
		late        bool
		cl          *client
		done, told  atomic.Bool
		lateMS      atomic.Int64
	}
	orders := make([]*order, 2*each)
	var wg sync.WaitGroup
	for i := range orders {
		o := &order{late: i%2 == 1,
			id:      fmt.Sprintf("late-%d-%d", stamp, i),
			session: fmt.Sprintf("demo-l-%d-%d", stamp, i)}
		orders[i] = o
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.cl, _ = r.c.open(graphLate, o.session)
		}()
	}
	wg.Wait()
	defer func() {
		for _, o := range orders {
			o.cl.close()
		}
	}()

	// Every session is open before the first order goes out, and the orders
	// go out one after another: what is being timed is the order, not forty
	// sessions being opened at once.
	for _, o := range orders {
		if o.cl == nil {
			continue
		}
		wait := 0
		if o.late {
			wait = delay
		}
		if _, err := o.cl.say(fmt.Sprintf("order %d %s", wait, o.id)); err != nil {
			continue
		}
		wg.Add(1)
		go func(o *order) {
			defer wg.Done()
			end := time.Now().Add(time.Duration(delay+900) * time.Millisecond)
			for time.Now().Before(end) {
				env, err := o.cl.read(time.Until(end))
				if err != nil {
					return
				}
				switch {
				case env.Kind == channel.KindData && textOf(env) == "done "+o.id:
					o.done.Store(true)
					return
				case env.Kind == channel.KindStatus:
					if event, late := eventOf(env); event == "deadline_exceeded" {
						o.told.Store(true)
						o.lateMS.Store(late)
						if !o.late {
							return
						}
					}
				}
			}
		}(o)
		time.Sleep(3 * time.Millisecond)
	}
	wg.Wait()
	st1, _ := r.c.stats()

	// Each order's fate, by three accounts that have to agree: the service's,
	// the actuator's, and the ledger's.
	var (
		onTime, missed           int // issued at once: carried out, or expired anyway
		lateExecuted, lateSealed int
		contradictions, unknown  int
		lateBy                   []int64
	)
	for _, o := range orders {
		executed := r.svc.carriedOut(o.id)
		var led struct {
			Entries []struct {
				Outcome string `json:"outcome"`
			} `json:"entries"`
		}
		r.c.call(http.MethodGet, "/v1/sessions/"+o.session+"/ledger", nil, &led)
		delivered, expired := 0, 0
		for _, e := range led.Entries {
			switch e.Outcome {
			case "delivered":
				delivered++
			case "expired":
				expired++
			}
		}
		switch {
		case executed && expired == 0 && delivered == 1 && r.cast.handed(o.id):
			if o.late {
				lateExecuted++
			} else {
				onTime++
			}
		case !executed && expired == 1 && delivered == 0 && !r.cast.handed(o.id):
			if o.late {
				lateSealed++
				if o.told.Load() {
					lateBy = append(lateBy, o.lateMS.Load())
				}
			} else {
				missed++
			}
		case executed || r.cast.handed(o.id):
			contradictions++ // acted on, without a clean delivered entry to show for it
		default:
			unknown++
		}
	}
	counted := uint64(0)
	if st0.Deadlines != nil && st1.Deadlines != nil {
		counted = st1.Deadlines.EffectsExpired - st0.Deadlines.EffectsExpired
	}

	b.fact("executed_on_time", onTime)
	b.fact("missed_on_time", missed)
	b.fact("executed_late", lateExecuted)
	b.fact("sealed_expired", lateSealed)
	if len(lateBy) > 0 {
		sort.Slice(lateBy, func(i, j int) bool { return lateBy[i] < lateBy[j] })
		b.fact("late_by_ms", lateBy[len(lateBy)/2])
	}

	b.check("no_late_order_was_carried_out", lateExecuted == 0,
		"%d late orders were carried out", lateExecuted)
	b.check("each_late_order_is_sealed_as_expired", lateSealed == each,
		"the ledger holds an expired entry for %d of %d late orders", lateSealed, each)
	b.check("every_order_has_exactly_one_fate", contradictions == 0 && unknown == 0,
		"%d orders were acted on without a matching entry, %d left no trace", contradictions, unknown)
	b.check("orders_in_time_were_carried_out", onTime > 0 && onTime+missed == each,
		"%d of %d on-time orders were carried out and %d expired", onTime, each, missed)
	b.check("the_node_counted_them", counted == uint64(lateSealed+missed),
		"the node counted %d expired effects; the ledger holds %d", counted, lateSealed+missed)
}

// ── 4. generations cut mid-sentence ─────────────────────────────────

// beatCut starts long generations and then has every client cancel at once. A
// cancel that only stops the answer being shown leaves the model working; what
// is measured here is when the engine was let go.
func (r *Run) beatCut(b *beat) {
	const (
		n       = cutGens
		planned = cutPlanned
	)
	stamp := time.Now().UnixNano() % 1_000_000
	st0, _ := r.c.stats()

	type gen struct {
		cl       *client
		prompt   string
		msg      string
		tokens   atomic.Int64
		cancelAt time.Time
	}
	gens := make([]*gen, 0, n)
	defer func() {
		for _, g := range gens {
			g.cl.close()
		}
	}()
	for i := 0; i < n; i++ {
		cl, err := r.c.open(graphTalk, fmt.Sprintf("demo-t-%d-%d", stamp, i))
		if err != nil {
			b.check("generations_started", false, "could not open a session: %v", err)
			return
		}
		g := &gen{cl: cl, prompt: fmt.Sprintf("talk %d cut-%d-%d", planned, stamp, i)}
		gens = append(gens, g)
		go func() {
			for {
				env, err := g.cl.read(30 * time.Second)
				if err != nil {
					return
				}
				if env.Kind == channel.KindData {
					g.tokens.Add(1)
				}
			}
		}()
	}
	for _, g := range gens {
		id, err := g.cl.say(g.prompt)
		if err != nil {
			b.check("generations_started", false, "could not start a generation: %v", err)
			return
		}
		g.msg = id
	}
	// Let every one of them be well into its answer.
	started := func() int {
		c := 0
		for _, g := range gens {
			if g.tokens.Load() >= 40 {
				c++
			}
		}
		return c
	}
	for end := time.Now().Add(8 * time.Second); started() < n && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(700 * time.Millisecond)
	if !b.check("generations_started", started() == n,
		"%d of %d generations were producing — %s", started(), n, loadString(&r.cast.inferErr)) {
		return
	}

	first := time.Now()
	prompts := make([]string, 0, n)
	for _, g := range gens {
		g.cancelAt = time.Now()
		_ = g.cl.cancel(g.msg)
		prompts = append(prompts, g.prompt)
	}

	b.fact("generations", n)
	b.fact("planned_tokens", n*planned)
	released := 0
	if r.eng != nil {
		// Seen at the engine: the moment each caller hung up.
		rel := r.eng.waitReleased(prompts, 5*time.Second)
		var took []float64
		produced := 0
		for _, g := range gens {
			if x, ok := rel[g.prompt]; ok {
				released++
				took = append(took, float64(x.at.Sub(g.cancelAt).Microseconds())/1000)
				produced += x.produced
			}
		}
		sort.Float64s(took)
		if len(took) > 0 {
			b.fact("release_ms_median", round(took[len(took)/2], 1))
			b.fact("release_ms_max", round(took[len(took)-1], 1))
		}
		b.fact("measured_at", "engine")
		b.fact("produced_tokens", produced)
		b.fact("saved_tokens", released*planned-produced)
	} else {
		// A real engine does not say when it was let go, so this is the node's
		// account: when it had no generation left in flight.
		took := time.Duration(0)
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
			if st, ok := r.c.stats(); ok && st.Inference != nil && st.Inference.InFlight == 0 {
				released, took = n, time.Since(first)
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		b.fact("release_ms_max", ms(took))
		b.fact("measured_at", "node")
	}
	time.Sleep(250 * time.Millisecond)
	st1, _ := r.c.stats()
	counted := st1.stopped("chain_cancelled") - st0.stopped("chain_cancelled")
	b.fact("released", released)

	b.check("every_generation_was_released", released == n,
		"%d of %d generations were let go within 5 s", released, n)
	b.check("the_node_recorded_why", counted >= uint64(n),
		"the node recorded %d cancelled chains for %d cancels", counted, n)
}

// ── 5. the node is killed ───────────────────────────────────────────

// beatKill kills the leading node with a signal it cannot handle, in the
// middle of everything, and times how long the system took to be whole again.
func (r *Run) beatKill(b *beat) {
	for end := time.Now().Add(8 * time.Second); !r.c.standbyWatching() && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.check("a_standby_was_watching", r.c.standbyWatching(),
		"the standby never reported who it was standing by for") {
		return
	}
	entries0, _ := r.c.ledgerEntries()
	interrupted0 := r.lanes.interrupted.Load()
	flows := append(append([]*flow{}, r.lanes.relay...), r.lanes.ops...)

	killedAt, err := r.c.killLeader()
	if !b.check("the_node_was_killed", err == nil, "%v", err) {
		return
	}
	r.mu.Lock()
	r.live.Node = "down"
	r.mu.Unlock()

	health, err := r.c.waitHealthy(20 * time.Second)
	outage := time.Since(killedAt)
	r.mu.Lock()
	r.nodeRole = "standby"
	r.mu.Unlock()
	b.fact("outage_ms", ms(outage))
	if !b.check("the_standby_answered", err == nil, "nothing answered on the node's port for 20 s") {
		return
	}
	node, _ := health["node"].(string)
	b.check("it_is_the_same_node", node == r.c.nodeID, "%s became %s", r.c.nodeID, node)

	expertiseBack := r.cast.waitConnected(15 * time.Second)
	b.fact("expertise_ms", ms(time.Since(killedAt)))
	b.check("every_expertise_reconnected", expertiseBack, "not all Expertise came back within 15 s")

	// Every session answering again, on its own, with its own id.
	back, last := 0, int64(0)
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); {
		back, last = 0, 0
		for _, f := range flows {
			if at := f.backAt.Load(); at > killedAt.UnixNano() {
				back++
				last = max(last, at)
			}
		}
		b.fact("sessions_back", back)
		if back == len(flows) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.fact("sessions", len(flows))
	if last > 0 {
		b.fact("back_ms", round(float64(last-killedAt.UnixNano())/1e6, 0))
	}
	b.check("every_session_resumed", back == len(flows),
		"%d of %d sessions were answering again after 30 s", back, len(flows))

	// The ledger goes on from where it was, rather than starting over.
	entries1 := entries0
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); {
		if n, ok := r.c.ledgerEntries(); ok && n > entries0 {
			entries1 = n
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.fact("effects_before", entries0)
	b.fact("effects_after", entries1)
	b.fact("interrupted", r.lanes.interrupted.Load()-interrupted0)
	b.fact("standby_said", r.c.tookOver() != "")
	b.check("the_ledger_continued", entries1 > entries0,
		"the ledger held %d entries before and %d after", entries0, entries1)
}

// ── 6. one byte of the ledger ───────────────────────────────────────

// verdict is what GET /v1/ledger/verify answers.
type verdict struct {
	Sound  bool `json:"sound"`
	Report struct {
		TotalEntries int64  `json:"total_entries"`
		ChainIntact  bool   `json:"chain_intact"`
		BrokenAtSeq  uint64 `json:"broken_at_seq"`
		BrokenReason string `json:"broken_reason"`
	} `json:"report"`
}

// beatTamper edits the ledger where it lives — the database file, not any
// interface the node offers — and asks whether anyone can tell.
func (r *Run) beatTamper(b *beat) {
	entries, ok := r.c.ledgerEntries()
	if !b.check("there_is_a_ledger_to_attack", ok && entries >= 3,
		"the ledger holds %d entries", entries) {
		return
	}
	var before verdict
	t := time.Now()
	code, _ := r.c.call(http.MethodGet, "/v1/ledger/verify", nil, &before)
	b.fact("verify_before_ms", ms(time.Since(t)))
	b.fact("entries", before.Report.TotalEntries)
	if !b.check("sound_before", code == 200 && before.Sound,
		"the ledger did not verify before it was touched: %d %s", code, before.Report.BrokenReason) {
		return
	}

	seq := uint64(entries / 2)
	edit, err := tamper(r.c.dir, seq)
	if !b.check("one_byte_was_edited", err == nil, "%v", err) {
		return
	}
	b.fact("edited_seq", seq)
	b.fact("edited_field", edit.field)
	b.fact("edited_from", edit.from)
	b.fact("edited_to", edit.to)

	var after verdict
	t = time.Now()
	code, _ = r.c.call(http.MethodGet, "/v1/ledger/verify", nil, &after)
	b.fact("verify_ms", ms(time.Since(t)))
	b.fact("broken_at_seq", after.Report.BrokenAtSeq)
	b.fact("broken_reason", after.Report.BrokenReason)

	offlineSound, out, took := r.c.verifyOffline()
	b.fact("offline_ms", ms(took))

	b.check("the_node_says_not_sound", code == http.StatusConflict && !after.Sound,
		"verification answered %d, sound=%v", code, after.Sound)
	b.check("it_names_the_entry", after.Report.BrokenAtSeq == seq || after.Report.BrokenAtSeq == seq+1,
		"entry %d was edited and verification stopped at %d", seq, after.Report.BrokenAtSeq)
	b.check("offline_verification_agrees", !offlineSound && strings.Contains(out, "NOT SOUND"),
		"`aura verify` did not report the ledger as unsound")
}

type edit struct{ field, from, to string }

// tamper changes one character of one sealed entry, directly in the database
// file. It picks a character inside a value, so the entry is still well-formed
// and simply says something that was never sealed.
func tamper(dataDir string, seq uint64) (edit, error) {
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "kernel.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return edit{}, err
	}
	defer db.Close()
	var entry string
	if err := db.QueryRow(`SELECT entry FROM ledger_entries WHERE seq = ?`, seq).Scan(&entry); err != nil {
		return edit{}, fmt.Errorf("read entry %d: %w", seq, err)
	}
	const field = "capability"
	key := `"` + field + `":"`
	at := strings.Index(entry, key)
	if at < 0 {
		return edit{}, fmt.Errorf("entry %d has no %s", seq, field)
	}
	start := at + len(key)
	end := start + strings.IndexByte(entry[start:], '"')
	if end <= start {
		return edit{}, fmt.Errorf("entry %d has an empty %s", seq, field)
	}
	from := entry[start:end]
	// The last letter of the capability: `motor.demo.dispatch` becomes a
	// capability that does not exist, one byte away.
	last := entry[end-1]
	swap := byte('x')
	if last == swap {
		swap = 'y'
	}
	changed := entry[:end-1] + string(swap) + entry[end:]
	res, err := db.Exec(`UPDATE ledger_entries SET entry = ? WHERE seq = ?`, changed, seq)
	if err != nil {
		return edit{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return edit{}, fmt.Errorf("entry %d was not there to edit", seq)
	}
	return edit{field: field, from: from, to: from[:len(from)-1] + string(swap)}, nil
}
