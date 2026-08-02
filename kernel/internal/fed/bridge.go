// Package fed implements node federation.
//
// A federation bridge is pure userland — nada de esto pide privilegios
// especiales al kernel. Es dos cosas al mismo tiempo: un skill-CLIENT del
// nodo local (registra proxy skills sobre /ws/skill, tal cual cualquier
// skill normal) y un stream-CLIENT de un nodo remoto (maneja grafos allá
// sobre /v1/stream). En el medio, relayea envelopes entre ambos — así una
// capability que físicamente vive en el nodo remoto se vuelve resolvable
// acá.
//
// Lo lindo de esto: leaf-node autonomy sale gratis. Cada nodo corre solo;
// un bridge únicamente amplía lo que el nodo local puede resolver. Cero
// cambios en el kernel — federation es infraestructura arriba de los
// contratos ya congelados, no una excepción a ellos.
//
// # What a bridge has to preserve
//
// A relay that forwards payloads and nothing else quietly voids the two
// guarantees this runtime is built on, because both are carried by envelope
// *metadata* rather than by payloads:
//
//   - **Cancellation.** C3 requires a cancel to reach every skill working on a
//     chain, at any depth, addressed with the cause_id that skill will
//     recognise. A bridge that drops `cancel` envelopes — as this one did —
//     terminates the chain at the node boundary: the local kernel suppresses
//     its own side, the remote skill keeps working, and its output keeps
//     arriving until it finishes. Barge-in across a federation simply did not
//     happen.
//   - **Idempotency.** C3 rule 2 makes `idem` the deduplication key for the
//     whole downstream chain. Minting a fresh one per relayed envelope — as
//     this one did — meant a retried delivery looked like new work to every
//     skill past the bridge, so at-least-once delivery became at-least-once
//     *execution* the moment a federation was involved.
//
// Both are now derived from the incoming envelope rather than invented, and
// cancels are forwarded in both directions.
package fed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

type remoteSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Capability  string `json:"capability"`
	Type        string `json:"type"`
	Ports       struct {
		Ingress []port `json:"ingress"`
		Egress  []port `json:"egress"`
	} `json:"ports"`
}

type port struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
}

// Bridge federates one remote node into the local node.
type Bridge struct {
	LocalWS    string // ws://localhost:<localport>/ws/skill
	LocalHTTP  string // http://localhost:<localport>
	RemoteHTTP string // http://<remote>:<port>
	RemoteWS   string // ws://<remote>:<port>
	CapFilter  string // "" = all; otherwise exact or prefix
	// LocalToken and RemoteToken authenticate the two ends. A bridge holds no
	// privilege of its own: it presents a node token exactly like any other
	// client of either node.
	LocalToken  string
	RemoteToken string
	Log         *slog.Logger

	// Route is what Run() measured about the path to the remote node —
	// same-host, LAN, or relay (see route.go). Set once at startup;
	// exported so `aura federate` and tests can observe it.
	Route RouteClass

	// pool holds one persistent /v1/stream connection per federated
	// capability, reused across every relay instead of dialed fresh per
	// envelope — see pooledConn and relay().
	poolMu sync.Mutex
	pool   map[string]*pooledConn
}

func (b *Bridge) localHeader() http.Header {
	return bearer(b.LocalToken)
}

func (b *Bridge) remoteHeader() http.Header {
	return bearer(b.RemoteToken)
}

func bearer(token string) http.Header {
	if token == "" {
		return nil
	}
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

// Run discovers remote skills and proxies each one, forever.
func (b *Bridge) Run(ctx context.Context) error {
	skills, err := b.remoteSkills()
	if err != nil {
		return fmt.Errorf("cannot reach remote node %s: %w", b.RemoteHTTP, err)
	}
	b.Route = classifyRoute(b.RemoteHTTP, b.measureRTT())
	proxied := 0
	var wg sync.WaitGroup
	for _, sk := range skills {
		if len(sk.Ports.Ingress) == 0 {
			continue
		}
		if b.CapFilter != "" && sk.Capability != b.CapFilter &&
			!strings.HasPrefix(sk.Capability, b.CapFilter+".") {
			continue
		}
		proxied++
		wg.Add(1)
		go func(s remoteSkill) {
			defer wg.Done()
			b.serveProxy(ctx, s)
		}(sk)
	}
	if proxied == 0 {
		return fmt.Errorf("remote node exposes no skills matching %q", b.CapFilter)
	}
	b.Log.Info("federation active", "remote", b.RemoteHTTP, "proxied", proxied, "route", b.Route)
	fmt.Printf("federating %s → %d skill(s) proxied into the local node (route: %s)\n",
		b.RemoteHTTP, proxied, b.Route)
	wg.Wait()
	return nil
}

// measureRTT times a handful of round trips to the remote's /healthz — the
// same endpoint every node already answers with no token required — and
// returns the fastest one, so one slow outlier (a cold TCP stack, first-
// connection DNS) does not misclassify an otherwise-fast route. Failing to
// measure at all degrades to RouteRelay via classifyRoute rather than
// aborting federation over what is only an observability signal.
func (b *Bridge) measureRTT() time.Duration {
	client := &http.Client{Timeout: 2 * time.Second}
	best := time.Duration(0)
	measured := false
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodGet, b.RemoteHTTP+"/healthz", nil)
		if err != nil {
			continue
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if elapsed := time.Since(start); !measured || elapsed < best {
			best, measured = elapsed, true
		}
	}
	if !measured {
		return time.Hour // deliberately large: classifyRoute falls through to RouteRelay
	}
	return best
}

func (b *Bridge) remoteSkills() ([]remoteSkill, error) {
	req, err := http.NewRequest(http.MethodGet, b.RemoteHTTP+"/v1/skills", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range b.remoteHeader() {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("remote node requires a token (set AURA_REMOTE_TOKEN)")
	}
	var out []remoteSkill
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// proxyManifest re-declares the remote skill locally. Capability is kept
// (so local resolution finds it); id and description are tagged as federated
// for traceability. Permissions are cleared — the proxy only relays.
func (b *Bridge) proxyManifest(s remoteSkill) map[string]any {
	host := strings.TrimPrefix(strings.TrimPrefix(b.RemoteHTTP, "http://"), "https://")
	ingress := make([]map[string]string, 0, len(s.Ports.Ingress))
	for _, p := range s.Ports.Ingress {
		ingress = append(ingress, map[string]string{"name": p.Name, "schema": p.Schema})
	}
	egress := make([]map[string]string, 0, len(s.Ports.Egress))
	for _, p := range s.Ports.Egress {
		egress = append(egress, map[string]string{"name": p.Name, "schema": p.Schema})
	}
	return map[string]any{
		"id":          "fed/" + strings.ReplaceAll(host, ":", "-") + "/" + lastSeg(s.ID),
		"version":     "1.0.0",
		"protocol":    "1",
		"name":        s.Name + " (federated)",
		"description": fmt.Sprintf("%s [federated from %s]", s.Description, host),
		"capability":  s.Capability,
		// The remote skill's type is preserved, and it matters: the local
		// node's policy decides what to do about an effect by capability and
		// type, so a federated `motor.*` must still look like one here. A proxy
		// that flattened this would let a federation launder an effect past the
		// gate.
		"type":        s.Type,
		"format":      "projection",
		"ports":       map[string]any{"ingress": ingress, "egress": egress},
		"permissions": map[string]any{"egress_http": []string{host}, "channels": "declared-only"},
	}
}

func lastSeg(id string) string {
	parts := strings.Split(id, "/")
	return parts[len(parts)-1]
}

// serveProxy keeps one proxy skill registered on the local node, relaying to
// the remote node, reconnecting forever.
func (b *Bridge) serveProxy(ctx context.Context, s remoteSkill) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := b.proxySession(ctx, s); err != nil && ctx.Err() == nil {
			b.Log.Debug("proxy reconnecting", "capability", s.Capability, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}

// relayState tracks the in-flight relays of one proxy session, so a cancel
// arriving for a chain can reach the remote node working on it.
//
// This is the federation-side counterpart of the kernel's causal index, and it
// exists for the same reason: the remote end knows the work by an id the local
// cancel does not carry, so something has to hold the mapping.
//
// Two things a cancel now has to do, where one used to be enough: unblock
// *this* relay's own wait loop immediately (stop, same as before — a plain
// context cancellation), and tell the *remote* node to actually stop, which
// on a connection shared with other relays (see pooledConn) can no longer be
// "close the socket" — that would abort every other relay sharing it. The
// remote id and pool are not known until relay() has dialed/reused a
// connection and sent its envelope, which can race a cancel that arrives
// first; setRemote resolves that race by sending the cancel immediately if
// one was already recorded.
type relayState struct {
	mu      sync.Mutex
	byLocal map[string]*trackedRelay
}

type trackedRelay struct {
	cancelled bool
	remoteID  string
	pool      *pooledConn
	stop      context.CancelFunc
}

func newRelayState() *relayState {
	return &relayState{byLocal: map[string]*trackedRelay{}}
}

func (r *relayState) track(localID string, stop context.CancelFunc) {
	if localID == "" {
		return
	}
	r.mu.Lock()
	r.byLocal[localID] = &trackedRelay{stop: stop}
	r.mu.Unlock()
}

func (r *relayState) setRemote(localID, remoteID string, pool *pooledConn) {
	r.mu.Lock()
	t, ok := r.byLocal[localID]
	if !ok {
		r.mu.Unlock()
		return
	}
	t.remoteID, t.pool = remoteID, pool
	cancelled := t.cancelled
	r.mu.Unlock()
	if cancelled {
		sendCancel(pool, remoteID)
	}
}

func (r *relayState) done(localID string) {
	r.mu.Lock()
	delete(r.byLocal, localID)
	r.mu.Unlock()
}

// cancel marks a tracked relay cancelled — sending the wire cancel envelope
// now if the remote id is already known, or leaving it for setRemote to send
// the moment it is — and unblocks the relay's own local wait loop. Reports
// whether anything was being tracked under this id at all.
func (r *relayState) cancel(localID string) bool {
	r.mu.Lock()
	t, ok := r.byLocal[localID]
	if !ok {
		r.mu.Unlock()
		return false
	}
	t.cancelled = true
	remoteID, pool, stop := t.remoteID, t.pool, t.stop
	r.mu.Unlock()
	if pool != nil {
		sendCancel(pool, remoteID)
	}
	if stop != nil {
		stop()
	}
	return true
}

func sendCancel(pool *pooledConn, remoteID string) {
	_ = pool.write(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: remoteID, Kind: channel.KindCancel,
	})
}

// pooledConn is one persistent /v1/stream connection to the remote node,
// shared across every relay for one federated capability — the fix for the
// dial-per-envelope cost this package used to accept as a given (see the
// package doc comment and ROADMAP.md, Phase 3). A background readLoop
// demultiplexes replies to whichever relay's channel is registered under
// the *local* envelope id that relay sent, the same problem relayState
// already solves for cancel routing, solved here for reply routing.
type pooledConn struct {
	conn *websocket.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	waiters map[string]chan channel.Envelope
	closed  bool
}

func (pc *pooledConn) readLoop(log *slog.Logger) {
	for {
		var reply channel.Envelope
		if pc.conn.ReadJSON(&reply) != nil {
			pc.closeAndDrain()
			return
		}
		pc.mu.Lock()
		ch, ok := pc.waiters[reply.CauseID]
		pc.mu.Unlock()
		if !ok {
			continue // nobody is waiting for this any more — already done or cancelled
		}
		select {
		case ch <- reply:
		default:
			// A slow (or already-gone) receiver must not stall every other
			// relay sharing this connection.
			log.Debug("federation: dropped a reply, receiver too slow or gone")
		}
	}
}

func (pc *pooledConn) closeAndDrain() {
	pc.mu.Lock()
	if pc.closed {
		pc.mu.Unlock()
		return
	}
	pc.closed = true
	waiters := pc.waiters
	pc.waiters = map[string]chan channel.Envelope{}
	pc.mu.Unlock()
	_ = pc.conn.Close()
	for _, ch := range waiters {
		close(ch)
	}
}

func (pc *pooledConn) isClosed() bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.closed
}

func (pc *pooledConn) register(id string) chan channel.Envelope {
	ch := make(chan channel.Envelope, 8)
	pc.mu.Lock()
	pc.waiters[id] = ch
	pc.mu.Unlock()
	return ch
}

func (pc *pooledConn) unregister(id string) {
	pc.mu.Lock()
	delete(pc.waiters, id)
	pc.mu.Unlock()
}

func (pc *pooledConn) write(env channel.Envelope) error {
	pc.writeMu.Lock()
	defer pc.writeMu.Unlock()
	return pc.conn.WriteJSON(env)
}

// pooledConnFor returns the shared connection for one capability, dialing
// and starting it the first time — or after a previous one died — rather
// than once per envelope. Holding poolMu across the dial serializes
// concurrent first-relays for the *same* capability (a one-time cost) in
// exchange for never leaking a duplicate connection; that trade favours
// correctness over the rare-case latency of a cold start.
func (b *Bridge) pooledConnFor(ctx context.Context, s remoteSkill, graphID string) (*pooledConn, error) {
	b.poolMu.Lock()
	defer b.poolMu.Unlock()

	if b.pool == nil {
		b.pool = map[string]*pooledConn{}
	}
	if pc, ok := b.pool[s.Capability]; ok && !pc.isClosed() {
		return pc, nil
	}

	conn, _, err := websocket.DefaultDialer.DialContext(
		ctx, b.RemoteWS+"/v1/stream?graph="+graphID, b.remoteHeader())
	if err != nil {
		return nil, err
	}
	var hello channel.Envelope
	if conn.ReadJSON(&hello) != nil || hello.Kind == channel.KindError {
		_ = conn.Close()
		return nil, fmt.Errorf("remote session rejected")
	}

	pc := &pooledConn{conn: conn, waiters: map[string]chan channel.Envelope{}}
	go pc.readLoop(b.Log)
	b.pool[s.Capability] = pc
	return pc, nil
}

func (b *Bridge) proxySession(ctx context.Context, s remoteSkill) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, b.LocalWS, b.localHeader())
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()

	reg := map[string]any{"v": "1", "id": channel.NewID(), "kind": "register",
		"payload": b.proxyManifest(s)}
	if err := conn.WriteJSON(reg); err != nil {
		return err
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		return err
	}
	if ack["kind"] == "error" {
		return fmt.Errorf("local node rejected proxy manifest: %v", ack["payload"])
	}

	// The remote graph is registered once per session rather than once per
	// envelope. It is a deterministic document keyed by capability, so posting
	// it on every message was pure overhead on the hot path — and a failed post
	// was being ignored, so the first delivery failed for a reason nothing
	// reported.
	graphID, err := b.ensureRemoteGraph(s)
	if err != nil {
		return err
	}

	state := newRelayState()
	var writeMu sync.Mutex
	for {
		var env channel.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return err
		}
		switch env.Kind {
		case channel.KindCancel:
			// The guarantee that used to stop at the node boundary. The local
			// kernel addresses this cancel with the cause_id *this proxy* knows
			// the work by, which is exactly the key the relay was tracked under.
			if state.cancel(env.CauseID) {
				b.Log.Debug("federated cancel propagated", "capability", s.Capability)
			}
		case channel.KindData:
			relayCtx, stop := context.WithCancel(ctx)
			state.track(env.ID, stop)
			go func(in channel.Envelope) {
				defer stop()
				defer state.done(in.ID)
				b.relay(relayCtx, in, s, graphID, conn, &writeMu, state)
			}(env)
		}
	}
}

// ensureRemoteGraph registers the relay graph on the remote node and returns
// its id.
func (b *Bridge) ensureRemoteGraph(s remoteSkill) (string, error) {
	ingress := s.Ports.Ingress[0]
	graphID := "fed-" + strings.NewReplacer(".", "_", "/", "_").Replace(s.Capability)

	edges := []map[string]any{{"from": "client.fed_out", "to": "s." + ingress.Name}}
	for _, eg := range s.Ports.Egress {
		// Every remote egress port lands on a distinct client port of the same
		// name. Collapsing them all onto text_in — as this did — meant a skill
		// whose transcript, status and audio ports carry different schemas had
		// them arrive indistinguishable at the far end.
		edges = append(edges, map[string]any{
			"from": "s." + eg.Name, "to": "client." + eg.Name,
		})
	}
	graph, _ := json.Marshal(map[string]any{
		"ir": "1", "graph_id": graphID, "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "s", "resolve": s.Capability}},
		"edges": edges,
	})

	req, err := http.NewRequest(http.MethodPost, b.RemoteHTTP+"/v1/graphs", bytesReader(graph))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range b.remoteHeader() {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("register relay graph on %s: %w", b.RemoteHTTP, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("remote node refused the relay graph (%d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return graphID, nil
}

// relay drives the capability on the remote node for one ingress envelope,
// forwarding every reply back to the local caller. It no longer owns a
// connection of its own — it borrows the capability's shared pooledConn
// (dialing it the first time, via pooledConnFor) and is handed replies
// through a channel that connection's single reader goroutine demultiplexes
// by envelope id, rather than reading a private socket directly.
func (b *Bridge) relay(ctx context.Context, in channel.Envelope, s remoteSkill,
	graphID string, local *websocket.Conn, writeMu *sync.Mutex, state *relayState) {

	ingress := s.Ports.Ingress[0]
	fallbackPort := "text_out"
	if len(s.Ports.Egress) > 0 {
		fallbackPort = s.Ports.Egress[0].Name
	}

	pc, err := b.pooledConnFor(ctx, s, graphID)
	if err != nil {
		b.emitError(in, fallbackPort, local, writeMu, "federation link failed: "+err.Error())
		return
	}

	out := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Node: "client", Port: "fed_out", Seq: 1,
		// Derived from the incoming envelope, not minted. This is the key the
		// remote node and everything past it deduplicate on, so a retry of the
		// same local work has to produce the same value here.
		Idem:   in.Idem + ":fed",
		Schema: ingress.Schema, Kind: channel.KindData, Payload: in.Payload,
	}

	replies := pc.register(out.ID)
	defer pc.unregister(out.ID)
	// Resolves the race against a cancel that arrives before this line runs
	// — see relayState's doc comment.
	state.setRemote(in.ID, out.ID, pc)

	if pc.write(out) != nil {
		b.emitError(in, fallbackPort, local, writeMu, "could not forward to remote")
		return
	}

	timer := time.NewTimer(120 * time.Second)
	defer timer.Stop()

	seq := uint64(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case reply, ok := <-replies:
			if !ok { // the pooled connection died
				if seq == 0 {
					b.emitError(in, fallbackPort, local, writeMu, "federation link lost")
				}
				return
			}
			switch reply.Kind {
			case channel.KindData, channel.KindError, channel.KindStatus:
				seq++
				// The reply's own port is preserved where the remote named one, so
				// a multi-port skill stays multi-port across the bridge.
				replyPort := reply.Port
				if replyPort == "" || replyPort == "text_in" {
					replyPort = fallbackPort
				}
				b.emit(in, replyPort, reply.Kind, reply.Schema, reply.Payload, seq, local, writeMu)
				if reply.Kind == channel.KindError {
					return
				}
			case channel.KindDone:
				seq++
				b.emit(in, fallbackPort, channel.KindDone, reply.Schema, nil, seq, local, writeMu)
				return
			}
		}
	}
}

func (b *Bridge) emit(in channel.Envelope, port, kind, schema string, payload json.RawMessage,
	seq uint64, local *websocket.Conn, writeMu *sync.Mutex) {
	env := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: in.ID,
		Session: in.Session, Node: in.Node, Port: port, Seq: seq,
		Idem:   fmt.Sprintf("%s:fed:%s:%d", in.Idem, port, seq),
		Schema: schema, Kind: kind, Payload: payload,
	}
	writeMu.Lock()
	_ = local.WriteJSON(env)
	writeMu.Unlock()
}

func (b *Bridge) emitError(in channel.Envelope, port string, local *websocket.Conn,
	writeMu *sync.Mutex, detail string) {
	payload, _ := json.Marshal(map[string]string{"state": "error", "detail": detail})
	b.emit(in, port, channel.KindError, "std/status@1", payload, 1, local, writeMu)
}
