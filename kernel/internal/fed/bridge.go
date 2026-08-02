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
	b.Log.Info("federation active", "remote", b.RemoteHTTP, "proxied", proxied)
	fmt.Printf("federating %s → %d skill(s) proxied into the local node\n", b.RemoteHTTP, proxied)
	wg.Wait()
	return nil
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
type relayState struct {
	mu      sync.Mutex
	byCause map[string]context.CancelFunc
}

func newRelayState() *relayState {
	return &relayState{byCause: map[string]context.CancelFunc{}}
}

func (r *relayState) track(causeID string, cancel context.CancelFunc) {
	if causeID == "" {
		return
	}
	r.mu.Lock()
	r.byCause[causeID] = cancel
	r.mu.Unlock()
}

func (r *relayState) done(causeID string) {
	r.mu.Lock()
	delete(r.byCause, causeID)
	r.mu.Unlock()
}

// cancel aborts the relay for a cause id and reports whether one was running.
func (r *relayState) cancel(causeID string) bool {
	r.mu.Lock()
	stop, ok := r.byCause[causeID]
	delete(r.byCause, causeID)
	r.mu.Unlock()
	if ok {
		stop()
	}
	return ok
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
				b.relay(relayCtx, in, s, graphID, conn, &writeMu)
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
// forwarding every reply back to the local caller.
func (b *Bridge) relay(ctx context.Context, in channel.Envelope, s remoteSkill,
	graphID string, local *websocket.Conn, writeMu *sync.Mutex) {

	ingress := s.Ports.Ingress[0]
	fallbackPort := "text_out"
	if len(s.Ports.Egress) > 0 {
		fallbackPort = s.Ports.Egress[0].Name
	}

	remote, _, err := websocket.DefaultDialer.DialContext(
		ctx, b.RemoteWS+"/v1/stream?graph="+graphID, b.remoteHeader())
	if err != nil {
		b.emitError(in, fallbackPort, local, writeMu, "federation link failed: "+err.Error())
		return
	}
	defer remote.Close()
	// Close the remote socket the moment the relay is cancelled, so a cancel
	// stops the remote node's work instead of merely stopping us listening
	// to it.
	go func() { <-ctx.Done(); remote.Close() }()

	deadline := time.Now().Add(120 * time.Second)
	_ = remote.SetReadDeadline(deadline)

	var hello channel.Envelope
	if remote.ReadJSON(&hello) != nil || hello.Kind == channel.KindError {
		b.emitError(in, fallbackPort, local, writeMu, "remote session rejected")
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
	if remote.WriteJSON(out) != nil {
		b.emitError(in, fallbackPort, local, writeMu, "could not forward to remote")
		return
	}

	seq := uint64(0)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		var reply channel.Envelope
		if remote.ReadJSON(&reply) != nil {
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
