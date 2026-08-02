// Package gateway is the single-port HTTP+WS surface of the kernel.
//
//	GET  /healthz                    — node, mode, protocol
//	GET  /v1/skills                  — live catalog (what a planner reads)
//	GET  /v1/graphs                  — stored graph ids
//	POST /v1/graphs                  — register a C2 IR document
//	GET  /v1/sessions/{id}/events    — causal event log (raw material for `aura why`)
//	WS   /ws/skill                   — skill IoC connection (first frame: register)
//	WS   /v1/stream?graph=<id>       — client connection (session per socket)
package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/executor"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/projection"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

type Gateway struct {
	Node *identity.Node
	Reg  *registry.Registry
	St   *store.Store
	Mgr  *executor.Manager
	Proj *projection.Manager
	Adm  *Admission
	// Ldg is the node's effect ledger (C4). Nil is accepted so a Gateway built
	// for a test that has nothing to do with attestation still constructs —
	// the ledger routes and the /healthz summary simply omit themselves.
	Ldg *ledger.Ledger
	MCP http.HandlerFunc // standard projection: skills as MCP tools
	Log *slog.Logger
	// Auth guards the control surface: bearer token plus the WebSocket origin
	// allowlist. Nil means an unauthenticated node, which only --no-auth
	// produces and which prints a warning at startup.
	Auth *Auth
	// ConfigFile is the optional --config file layer: skill id -> declared
	// key -> value. Lower priority than a store override, higher than a
	// manifest default. Nil/empty means no file was given.
	ConfigFile map[string]map[string]any

	// Inbound-ingress bookkeeping, built on first use so a Gateway stays
	// constructible as a plain struct literal (which every test does).
	reaperOnce sync.Once
	reaper     *sessionReaper
	rateOnce   sync.Once
	rate       *routeRate
	routesOnce sync.Once
	routes     *routeCache
}

// upgrader builds the WebSocket upgrader for this node. It is per-Gateway
// rather than a package-level var because the origin check is now a policy
// decision the node owns — the old global answered `return true` to every
// origin, which meant any web page could drive a local node.
func (g *Gateway) upgrader() websocket.Upgrader {
	check := func(*http.Request) bool { return true }
	if g.Auth != nil {
		check = g.Auth.CheckOrigin
	}
	return websocket.Upgrader{
		ReadBufferSize:  1 << 16,
		WriteBufferSize: 1 << 16,
		CheckOrigin:     check,
	}
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", g.ui)
	mux.HandleFunc("GET /.well-known/agent.json", g.agentCard)
	if g.MCP != nil {
		mux.HandleFunc("POST /mcp", g.MCP)
		mux.HandleFunc("GET /mcp", g.MCP) // handler answers 405 (no SSE stream)
	}
	mux.HandleFunc("GET /healthz", g.health)
	mux.HandleFunc("GET /v1/skills", g.listSkills)
	mux.HandleFunc("GET /v1/skills/config", g.getSkillConfig)
	mux.HandleFunc("PUT /v1/skills/config", g.putSkillConfig)
	mux.HandleFunc("GET /v1/graphs", g.listGraphs)
	mux.HandleFunc("GET /v1/graphs/{id}", g.getGraph)
	mux.HandleFunc("POST /v1/graphs", g.registerGraph)
	mux.HandleFunc("GET /v1/sessions", g.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", g.sessionMeta)
	mux.HandleFunc("GET /v1/sessions/{id}/events", g.sessionEvents)
	mux.HandleFunc("POST /v1/projections", g.connectProjection)
	mux.HandleFunc("GET /v1/projections", g.listProjections)
	mux.HandleFunc("POST /v1/projections/{name}/promote", g.promoteOperation)
	mux.HandleFunc("POST /v1/ingress", g.declareIngress)
	mux.HandleFunc("GET /v1/ingress", g.listIngress)
	mux.HandleFunc("DELETE /v1/ingress/{name}", g.deleteIngress)
	mux.HandleFunc("GET /v1/ledger", g.listLedger)
	mux.HandleFunc("GET /v1/ledger/verify", g.verifyLedger)
	mux.HandleFunc("POST /hooks/{name}", g.receiveHook)
	mux.HandleFunc("GET /ws/skill", g.skillWS)
	mux.HandleFunc("GET /v1/stream", g.clientWS)

	// Auth wraps the whole mux rather than decorating each route: a route
	// added later is authenticated by default, which is the failure mode worth
	// designing for. /healthz and the inbound hooks opt out inside the
	// middleware — hooks carry their own HMAC and are called by systems that
	// will never hold a node token.
	if g.Auth != nil {
		return g.Auth.Authenticate(mux)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{
		"ok": true, "node": g.Node.ID, "mode": g.Node.Mode,
		"protocol": channel.ProtocolMajor, "ir": executor.IRMajor,
	}
	if budget, used := g.Adm.Snapshot(); budget > 0 {
		out["admission"] = map[string]string{
			"budget": FormatBytes(budget), "used": FormatBytes(used),
		}
	}
	// Cheap enough to compute on every probe (see ledger.Summarize), and
	// worth doing: an operator staring at an orchestrator's health checks
	// should not have to reach for `aura verify` just to see the ledger is
	// growing at all.
	if g.Ldg != nil {
		if sum, err := g.Ldg.Summarize(); err == nil {
			out["ledger"] = sum
		}
	}
	writeJSON(w, 200, out)
}

func (g *Gateway) listSkills(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(g.Reg.CatalogJSON())
}

// resolveManifest finds a skill's manifest whether it is currently
// connected (live registry) or not (last one the store saw) — config can
// be inspected/edited either way; it just won't reach a disconnected
// skill until it reconnects.
func (g *Gateway) resolveManifest(id string) (registry.Manifest, error) {
	if live, err := g.Reg.Resolve(id, ""); err == nil {
		return live.Manifest, nil
	}
	raw, err := g.St.LoadSkillManifest(id)
	if err != nil {
		return registry.Manifest{}, err
	}
	var m registry.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return registry.Manifest{}, fmt.Errorf("stored manifest for %q is corrupt: %w", id, err)
	}
	return m, nil
}

// effectiveConfig merges declared defaults, the --config file, and stored
// runtime overrides — see registry.Manifest.EffectiveConfig for precedence.
func (g *Gateway) effectiveConfig(m registry.Manifest) (map[string]any, error) {
	storeVals, err := g.St.LoadSkillConfig(m.ID)
	if err != nil {
		return nil, err
	}
	return m.EffectiveConfig(g.ConfigFile[m.ID], storeVals), nil
}

// getSkillConfig returns a skill's declared config schema plus its current
// effective values (GET /v1/skills/config?id=<skill id>).
func (g *Gateway) getSkillConfig(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "missing ?id="})
		return
	}
	m, err := g.resolveManifest(id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	values, err := g.effectiveConfig(m)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "schema": m.Config, "values": values,
	})
}

// putSkillConfig validates and stores a partial set of config overrides,
// then pushes the new effective config to any live connection of that
// skill id (PUT /v1/skills/config?id=<skill id>, JSON body: {key: value}).
func (g *Gateway) putSkillConfig(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "missing ?id="})
		return
	}
	m, err := g.resolveManifest(id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	for key, val := range patch {
		if err := m.ValidateConfigValue(key, val); err != nil {
			writeJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
	}
	stored, err := g.St.LoadSkillConfig(id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	for key, val := range patch {
		stored[key] = val
	}
	if err := g.St.SaveSkillConfig(id, stored); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	effective := m.EffectiveConfig(g.ConfigFile[m.ID], stored)

	payload, _ := json.Marshal(effective)
	env := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
		Kind: channel.KindConfigUpdate, Payload: payload}
	raw, _ := json.Marshal(env)
	pushed := g.Reg.SendToID(id, raw)

	writeJSON(w, 200, map[string]any{
		"id": id, "values": effective, "pushed_live": pushed > 0,
	})
}

func (g *Gateway) listGraphs(w http.ResponseWriter, _ *http.Request) {
	ids, err := g.St.ListGraphs()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"graphs": ids})
}

func (g *Gateway) getGraph(w http.ResponseWriter, r *http.Request) {
	ir, err := g.St.LoadGraph(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(ir)
}

func (g *Gateway) registerGraph(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	graph, err := executor.ParseGraph(raw)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	if err := g.St.SaveGraph(graph.GraphID, raw); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, map[string]string{"graph_id": graph.GraphID})
}

func (g *Gateway) listSessions(w http.ResponseWriter, _ *http.Request) {
	list, err := g.St.ListSessions(50)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": list})
}

func (g *Gateway) sessionMeta(w http.ResponseWriter, r *http.Request) {
	meta, err := g.St.GetSession(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, meta)
}

func (g *Gateway) sessionEvents(w http.ResponseWriter, r *http.Request) {
	events, times, err := g.St.SessionEvents(r.PathValue("id"), 0)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"session": r.PathValue("id"), "events": events, "timestamps": times,
	})
}

// agentCard serves an A2A discovery card describing this node's skills
// (discovery only; the A2A message protocol is future work).
func (g *Gateway) agentCard(w http.ResponseWriter, r *http.Request) {
	type cardSkill struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	}
	var skills []cardSkill
	for _, m := range g.Reg.Catalog() {
		skills = append(skills, cardSkill{
			ID: m.ID, Name: m.Name, Description: m.Description,
			Tags: []string{m.Type, m.Capability},
		})
	}
	writeJSON(w, 200, map[string]any{
		"name":               "aura node " + g.Node.ID,
		"description":        "AURA distributed cognitive runtime node",
		"url":                "http://" + r.Host,
		"version":            spec.Version,
		"defaultInputModes":  []string{"text/plain", "application/json"},
		"defaultOutputModes": []string{"text/plain", "application/json"},
		"capabilities":       map[string]any{"streaming": true},
		"skills":             skills,
	})
}

// Connection liveness. A half-dead TCP connection — the peer's machine
// slept, its wifi dropped, the process was SIGKILLed — never produces a read
// error, so without this the kernel keeps the session, its admission
// reservation and its registry entry forever. We ping every pingPeriod and
// declare the peer gone after pongWait without any inbound traffic.
//
// pingPeriod must stay comfortably below pongWait so a single lost ping is
// not fatal. Variables rather than constants so tests can shorten them.
var (
	pongWait   = 60 * time.Second
	pingPeriod = 20 * time.Second
	// How long a producer waits for a full reliable lane before giving up.
	// Realtime never waits at all — see sendRealtime.
	backpressureTimeout = 5 * time.Second
)

// wsWriter serializes writes to one websocket (C3 backpressure: bounded
// buffer; blocks the producer when full — QoS reliable). On Close it drains
// queued frames before the socket goes away, so terminal error envelopes
// are never lost to the close race.
//
// Pings go through here too: gorilla/websocket permits only one concurrent
// writer per connection, so a ping written from a ticker goroutine would
// race with envelope delivery.
type wsWriter struct {
	conn     *websocket.Conn
	ch       chan []byte // reliable: large, blocks the producer when full
	rt       chan []byte // realtime: small, drops the oldest, never blocks
	ping     chan struct{}
	once     sync.Once
	done     chan struct{}
	finished chan struct{}
}

const (
	reliableLane = 256
	// Sized in time, not in frames: at ~200ms of audio per chunk this holds
	// about 3 seconds. The reliable lane at the same rate would hold nearly a
	// minute, and you cannot interrupt speech that has already been queued —
	// no matter how good cancellation is upstream.
	realtimeLane = 16
	// A busy audio stream must not starve the reliable lane, where a `done`,
	// an `error` and an approval request travel.
	maxRealtimeRun = 8
)

func newWSWriter(conn *websocket.Conn) *wsWriter {
	w := &wsWriter{
		conn: conn,
		ch:   make(chan []byte, reliableLane),
		rt:   make(chan []byte, realtimeLane),
		ping: make(chan struct{}, 1),
		done: make(chan struct{}), finished: make(chan struct{}),
	}
	write := func(raw []byte) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.TextMessage, raw) == nil
	}
	go func() {
		defer close(w.finished)
		realtimeRun := 0
		for {
			// Realtime first — a late audio frame is a worse outcome than a
			// late status — but only up to maxRealtimeRun in a row.
			if realtimeRun < maxRealtimeRun {
				select {
				case raw := <-w.rt:
					realtimeRun++
					if !write(raw) {
						w.signalClose()
						return
					}
					continue
				default:
				}
			}
			realtimeRun = 0

			select {
			case raw := <-w.ch:
				if !write(raw) {
					w.signalClose()
					return
				}
			case raw := <-w.rt:
				realtimeRun++
				if !write(raw) {
					w.signalClose()
					return
				}
			case <-w.ping:
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if conn.WriteMessage(websocket.PingMessage, nil) != nil {
					w.signalClose()
					return
				}
			case <-w.done:
				// Drain the reliable lane only: a terminal error envelope must
				// survive the close race, a stale audio frame is not worth
				// delaying it for.
				for {
					select {
					case raw := <-w.ch:
						if !write(raw) {
							return
						}
					default:
						return
					}
				}
			}
		}
	}()
	return w
}

// Ping queues a websocket ping. Non-blocking: if one is already queued the
// peer is already behind, and stacking pings would not help.
func (w *wsWriter) Ping() {
	select {
	case w.ping <- struct{}{}:
	default:
	}
}

// keepAlive arms the read deadline and pings until the connection closes.
// Any inbound frame — a pong, or ordinary traffic from a busy peer — extends
// the deadline, so an actively streaming peer is never killed for not ponging
// promptly. Returns a func the caller defers to stop the ticker.
func keepAlive(conn *websocket.Conn, w *wsWriter) func() {
	extend := func() error { return conn.SetReadDeadline(time.Now().Add(pongWait)) }
	_ = extend()
	conn.SetPongHandler(func(string) error { return extend() })

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(pingPeriod)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.Ping()
			case <-stop:
				return
			case <-w.done:
				return
			}
		}
	}()
	return func() { close(stop) }
}

// Send queues a frame on the lane its edge declared. Anything that is not
// explicitly realtime is treated as reliable, including `bulk`, whose
// behaviour C3 names but does not define.
func (w *wsWriter) Send(raw []byte, qos string) error {
	if qos == channel.QoSRealtime {
		return w.sendRealtime(raw)
	}
	select {
	case w.ch <- raw:
		return nil
	case <-w.done:
		return fmt.Errorf("connection closed")
	case <-time.After(backpressureTimeout):
		return fmt.Errorf("backpressure timeout: receiver too slow")
	}
}

// sendRealtime never blocks and never reports failure. Under pressure it drops
// the OLDEST queued frame: in a live stream the stale frame is the worthless
// one, and a producer of audio must never be stalled by a slow consumer.
//
// This is also why connection liveness had to land first — a lane that never
// blocks and never errors removes the backpressure timeout, which used to be
// the only thing that noticed a half-dead peer.
func (w *wsWriter) sendRealtime(raw []byte) error {
	for attempt := 0; attempt <= realtimeLane; attempt++ {
		select {
		case w.rt <- raw:
			return nil
		case <-w.done:
			return fmt.Errorf("connection closed")
		default:
		}
		select {
		case <-w.rt: // make room by discarding the oldest
		default:
		}
	}
	return nil // still full against a live producer: drop this frame, not the stream
}

func (w *wsWriter) signalClose() { w.once.Do(func() { close(w.done) }) }

// Close stops the writer, waiting briefly for the drain to finish.
func (w *wsWriter) Close() {
	w.signalClose()
	select {
	case <-w.finished:
	case <-time.After(2 * time.Second):
	}
}

// skillWS handles a skill IoC connection. First frame MUST be kind=register.
func (g *Gateway) skillWS(w http.ResponseWriter, r *http.Request) {
	up := g.upgrader()
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	writer := newWSWriter(conn)
	defer writer.Close()
	defer keepAlive(conn, writer)()

	connID := channel.NewID()
	fail := func(cause, msg string) {
		payload, _ := json.Marshal(map[string]string{"state": "error", "detail": msg})
		env := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
			CauseID: cause, Kind: channel.KindError, Payload: payload}
		raw, _ := json.Marshal(env)
		_ = writer.Send(raw, channel.QoSReliable)
	}

	// Registration handshake.
	_, first, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var regEnv channel.Envelope
	if err := json.Unmarshal(first, &regEnv); err != nil || regEnv.Kind != channel.KindRegister {
		fail("", "first frame must be a C3 register envelope")
		return
	}
	var manifest registry.Manifest
	if err := json.Unmarshal(regEnv.Payload, &manifest); err != nil {
		fail(regEnv.ID, "register payload is not a C1 manifest: "+err.Error())
		return
	}
	if err := manifest.Validate(); err != nil {
		fail(regEnv.ID, "manifest rejected: "+err.Error())
		return
	}
	if manifest.Protocol != channel.ProtocolMajor {
		// N-2 window will land with protocol negotiation; today majors must match.
		fail(regEnv.ID, "protocol major mismatch")
		return
	}

	// R15 admission: reserve the declared memory or explain the refusal.
	declaredMem, _ := manifest.Requirements["memory"].(string)
	if err := g.Adm.Admit(connID, manifest.ID, declaredMem); err != nil {
		fail(regEnv.ID, err.Error())
		return
	}
	defer g.Adm.Release(connID)

	live := &registry.Live{Manifest: manifest, Connected: time.Now(), Send: writer.Send}
	g.Reg.Register(connID, live)
	defer g.Reg.Unregister(connID)
	if err := g.St.UpsertSkill(manifest.ID, manifest.Version, manifest); err != nil {
		g.Log.Error("persist skill failed", "err", err)
	}
	g.Log.Info("skill registered", "skill", manifest.ID, "type", manifest.Type,
		"capability", manifest.Capability)

	effective, err := g.effectiveConfig(manifest)
	if err != nil {
		g.Log.Error("load skill config failed", "skill", manifest.ID, "err", err)
		effective = map[string]any{}
	}
	ack, _ := json.Marshal(map[string]any{
		"state": "registered", "skill": manifest.ID, "config": effective,
	})
	ackEnv := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
		CauseID: regEnv.ID, Kind: channel.KindStatus, Payload: ack}
	rawAck, _ := json.Marshal(ackEnv)
	_ = writer.Send(rawAck, channel.QoSReliable)

	// Emission loop: everything the skill emits is dispatched to its session.
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			g.Log.Info("skill disconnected", "skill", manifest.ID, "err", err)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		var env channel.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			fail("", "invalid envelope: "+err.Error())
			continue
		}
		if err := env.Validate(); err != nil {
			fail(env.ID, err.Error())
			continue
		}
		if err := g.Mgr.Dispatch(env); err != nil {
			g.Log.Debug("dispatch failed", "skill", manifest.ID, "err", err)
		}
	}
}

// clientWS handles an application/user connection: one session per socket.
func (g *Gateway) clientWS(w http.ResponseWriter, r *http.Request) {
	graphID := r.URL.Query().Get("graph")
	if graphID == "" {
		http.Error(w, "missing ?graph=<graph_id>", http.StatusBadRequest)
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		sessionID = identity.NewSessionID()
	}

	up := g.upgrader()
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	writer := newWSWriter(conn)
	defer writer.Close()
	defer keepAlive(conn, writer)()

	sess, err := g.Mgr.Start(sessionID, graphID, writer.Send)
	if err != nil {
		payload, _ := json.Marshal(map[string]string{"state": "error", "detail": err.Error()})
		env := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
			Kind: channel.KindError, Payload: payload}
		raw, _ := json.Marshal(env)
		_ = writer.Send(raw, channel.QoSReliable)
		return
	}
	defer g.Mgr.End(sessionID)

	hello, _ := json.Marshal(map[string]string{"state": "ready", "session": sessionID, "graph": graphID})
	helloEnv := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
		Session: sessionID, Kind: channel.KindStatus, Payload: hello}
	rawHello, _ := json.Marshal(helloEnv)
	_ = writer.Send(rawHello, channel.QoSReliable)

	var seq uint64
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			g.Log.Info("client disconnected", "session", sessionID, "err", err)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		env, perr := parseClientFrame(raw, sessionID, &seq)
		if perr != nil {
			payload, _ := json.Marshal(map[string]string{"state": "error", "detail": perr.Error()})
			e := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
				Session: sessionID, Kind: channel.KindError, Payload: payload}
			rr, _ := json.Marshal(e)
			_ = writer.Send(rr, channel.QoSReliable)
			continue
		}
		sess.Route(env)
	}
}

// parseClientFrame accepts either a full C3 envelope or the shorthand
// {"text": "..."} — the SDK-less path for curl/browser users (R8).
func parseClientFrame(raw []byte, sessionID string, seq *uint64) (channel.Envelope, error) {
	var env channel.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("invalid json: %w", err)
	}
	if env.V != "" { // full envelope path
		env.Session = sessionID // the socket owns the session; never trust the frame
		if env.Node == "" {
			env.Node = executor.ClientRef
		}
		return env, env.Validate()
	}
	// Shorthand path: wrap into a std/text@1 envelope.
	var shorthand struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &shorthand); err != nil || strings.TrimSpace(shorthand.Text) == "" {
		return env, fmt.Errorf(`frame must be a C3 envelope or {"text": "..."}`)
	}
	*seq++
	payload, _ := json.Marshal(map[string]any{"text": shorthand.Text, "final": true})
	env = channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: sessionID,
		Node: executor.ClientRef, Port: "text_out", Seq: *seq,
		Idem:   fmt.Sprintf("%s:client:text_out:%d", sessionID, *seq),
		Schema: "std/text@1", Kind: channel.KindData, Payload: payload,
	}
	return env, nil
}
