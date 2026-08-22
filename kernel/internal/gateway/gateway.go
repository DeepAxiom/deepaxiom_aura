// Package gateway is the single-port HTTP+WS surface of the kernel.
//
//	GET  /healthz                    — node, mode, protocol
//	GET  /v1/skills                  — live catalog (what a planner reads)
//	GET  /v1/graphs                  — stored graph ids
//	POST /v1/graphs                  — register a C2 IR document
//	GET  /v1/graphs/{id}/revisions   — every version ever registered, newest first
//	GET  /v1/graphs/{id}/revisions/{n} — one version's IR
//	GET  /v1/sessions/{id}/events    — causal event log (raw material for `aura why`)
//	GET  /v1/sessions/{id}/ledger    — one session's sealed effects (`aura undo`)
//	GET  /v1/ledger/entries/{hash}   — one sealed effect by receipt (`aura undo`)
//	POST /v1/skills/wasm             — register a format:wasm skill, hosted in-process
//	WS   /ws/skill                   — skill IoC connection (first frame: register)
//	WS   /v1/stream?graph=<id>       — client connection (session per socket)
package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/approvals"
	"aura/kernel/internal/broker"
	"aura/kernel/internal/channel"
	"aura/kernel/internal/executor"
	"aura/kernel/internal/grammar"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/projection"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
	"aura/kernel/internal/wasmrt"
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
	// Wit is this node acting as a witness for *other* nodes' ledgers (C4
	// v1.2, external anchoring). Separate from Ldg because the two are
	// genuinely different roles: Ldg is this node's own history, Wit is what
	// it has promised about someone else's. A node can have one without the
	// other — a pure witness seals no effects of its own, and a node with no
	// keypair cannot witness. Nil answers 404 on POST /v1/ledger/witness.
	Wit *ledger.Witness
	// Grammars compiles a port's declared schema into a decoding grammar and
	// validates payloads against it (C1 typed ports, made enforceable). Nil
	// is accepted so a Gateway built for an unrelated test still constructs;
	// the schema routes then answer 404 and validation is skipped.
	Grammars *grammar.Registry
	// Wasm hosts every `format: wasm` skill on this node (Phase 3). Nil is
	// accepted for the same reason Ldg's nil is: a Gateway built for a test
	// unrelated to wasm skills should not have to wire a wazero runtime just
	// to construct — POST /v1/skills/wasm answers 404 instead.
	Wasm *wasmrt.Runtime
	MCP  http.HandlerFunc // standard projection: skills as MCP tools
	// Approvals is the queue of human-approval gates raised by a client that
	// is not a human — the MCP server above being the one that forced it to
	// exist. Nil is accepted: the approval routes then answer 404 and a gate
	// reaching a programmatic client resolves the old way, as a denial.
	Approvals *approvals.Registry
	// Broker is the credential broker: this node's secrets, released only
	// against the receipt of an effect that passed the Effect Checkpoint. Nil
	// answers 404 on the secret routes, which is the correct state for a node
	// whose connectors authenticate with nothing.
	Broker *broker.Broker
	Log    *slog.Logger
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
	// OpenEnv border state: live episodes, built on first use so a
	// Gateway stays constructible as a plain struct literal.
	openEnvOnce sync.Once
	openEnvReg  *openEnvRegistry
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

	// routes is the set of paths this gateway actually serves, built by the
	// same call that registers them. See the comment on APIRoutes below for
	// why it has to be built this way rather than listed separately.
	routes := map[string]bool{}
	handle := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
		path := pattern
		if _, rest, ok := strings.Cut(pattern, " "); ok {
			path = rest // patterns are "METHOD /path"
		}
		routes[path] = true
	}

	handle("GET /", g.ui)
	handle("GET /.well-known/agent.json", g.agentCard)
	handle("GET /openenv/spec", g.openEnvSpec)
	handle("POST /openenv/reset", g.openEnvReset)
	handle("POST /openenv/step", g.openEnvStep)
	handle("GET /openenv/state", g.openEnvState)
	handle("GET /openenv/bundle", g.openEnvBundle)
	if g.MCP != nil {
		handle("POST /mcp", g.MCP)
		handle("GET /mcp", g.MCP) // handler answers 405 (no SSE stream)
	}
	handle("GET /healthz", g.health)
	handle("GET /readyz", g.readiness)
	handle("GET /metrics", g.metrics)
	handle("GET /v1/approvals", g.listApprovals)
	handle("POST /v1/approvals/{id}", g.resolveApproval)
	handle("GET /v1/operators", g.listOperators)
	handle("POST /v1/operators", g.enrollOperator)
	handle("DELETE /v1/operators/{id}", g.revokeOperator)
	handle("GET /v1/secrets", g.listSecrets)
	handle("PUT /v1/secrets", g.putSecret)
	handle("DELETE /v1/secrets/{name}", g.deleteSecret)
	handle("POST /v1/secrets/resolve", g.resolveSecret)
	handle("GET /v1/skills", g.listSkills)
	handle("GET /v1/skills/config", g.getSkillConfig)
	handle("PUT /v1/skills/config", g.putSkillConfig)
	handle("GET /v1/graphs", g.listGraphs)
	handle("GET /v1/graphs/{id}", g.getGraph)
	handle("GET /v1/graphs/{id}/revisions", g.listGraphRevisions)
	handle("GET /v1/graphs/{id}/revisions/{n}", g.getGraphRevision)
	handle("POST /v1/graphs", g.registerGraph)
	handle("GET /v1/sessions", g.listSessions)
	handle("GET /v1/sessions/{id}", g.sessionMeta)
	handle("GET /v1/sessions/{id}/events", g.sessionEvents)
	handle("GET /v1/sessions/{id}/ledger", g.sessionLedger)
	handle("GET /v1/sessions/{id}/bundle", g.ledgerBundle)
	handle("POST /v1/projections", g.connectProjection)
	handle("GET /v1/projections", g.listProjections)
	handle("POST /v1/projections/{name}/promote", g.promoteOperation)
	handle("POST /v1/ingress", g.declareIngress)
	handle("GET /v1/ingress", g.listIngress)
	handle("DELETE /v1/ingress/{name}", g.deleteIngress)
	handle("GET /v1/schemas", g.listSchemas)
	handle("GET /v1/grammars/{ref...}", g.getGrammar)
	handle("GET /v1/schemas/{ref...}", g.getSchema)
	handle("GET /v1/ledger", g.listLedger)
	handle("GET /v1/ledger/verify", g.verifyLedger)
	handle("GET /v1/ledger/entries/{hash}", g.ledgerEntry)
	handle("GET /v1/ledger/head", g.ledgerHead)
	handle("GET /v1/ledger/statement", g.ledgerStatement)
	handle("POST /v1/ledger/witness", g.ledgerWitness)
	handle("GET /v1/ledger/witness/last-seen", g.ledgerWitnessLastSeen)
	handle("POST /v1/ledger/witness/record", g.ledgerRecordWitness)
	// The witness's own log (C4 v1.4) — read-only, and public on an open
	// witness, because a log only its operator can read is not auditable.
	handle("GET /v1/witness/head", g.witnessLogHead)
	handle("GET /v1/witness/log", g.witnessLogEntries)
	handle("GET /v1/witness/consistency", g.witnessLogConsistency)
	handle("GET /v1/witness/proof/{seq}", g.witnessLogInclusion)
	// This node following someone else's witness. Authenticated: it is our own
	// audit baseline, and a stranger who could lower it could erase the record
	// that would convict a witness.
	handle("GET /v1/witness/seen", g.witnessSeen)
	handle("POST /v1/witness/seen", g.witnessRecordSeen)
	handle("GET /v1/ledger/receipt/{hash}", g.ledgerReceipt)
	handle("GET /v1/ledger/attestations/{hash}", g.ledgerAttestation)
	handle("POST /v1/skills/wasm", g.registerWasmSkill)
	handle("POST /hooks/{name}", g.receiveHook)
	handle("GET /ws/skill", g.skillWS)
	handle("GET /v1/stream", g.clientWS)

	// Auth wraps the whole mux rather than decorating each route: a route
	// added later is authenticated by default, which is the failure mode worth
	// designing for. /healthz and the inbound hooks opt out inside the
	// middleware — hooks carry their own HMAC and are called by systems that
	// will never hold a node token.
	//
	// The middleware is installed even when there is no Auth, and that is not a
	// no-op: it is also what attaches the principal every downstream scope check
	// reads. Skipping it on an unauthenticated node would leave those checks
	// looking at the zero principal — nobody — and a node started with
	// --no-auth would refuse to register a single skill. One place decides who
	// a caller is, and it runs on every request.
	auth := g.Auth
	if auth == nil {
		auth = &Auth{}
	}
	// Tell the authenticator which paths are routes rather than app assets.
	//
	// `routes` is populated by handle() above, so this really is the mux's own
	// registrations and not a second list kept in step by hand. That
	// distinction is the whole point: isUIAsset serves anything it does not
	// recognise as a route without a token, because the browser has to be able
	// to load the app shell before it can present one. A route this map does
	// not know about is therefore a route with no authentication — which is
	// exactly the bug isUIAsset's comment describes, in a new place.
	//
	// The previous version claimed this derivation in a comment while assigning
	// a hand-written map of five paths sitting fifteen lines below, with
	// nothing testing the two against each other. It happened to be correct,
	// because every route added since lived under /v1/, /ws/, /openenv/ or
	// /hooks/ and those prefixes are matched separately. The first top-level
	// route added outside them would have been served to anyone.
	//
	// Wildcard patterns land here with their braces intact ("/v1/graphs/{id}"),
	// which matches no real request path — harmless, because every wildcard
	// route is under a prefix isUIAsset already refuses.
	auth.APIRoutes = routes
	return auth.Authenticate(mux)
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

// listGraphRevisions serves a graph's history, newest first.
//
// Bodies are omitted: a listing is for choosing, and a long-lived graph's
// history can be larger than the graph by a wide margin. The digest is enough
// to tell two versions apart, and to tell that two are the same.
func (g *Gateway) listGraphRevisions(w http.ResponseWriter, r *http.Request) {
	revs, err := g.St.GraphRevisions(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"graph_id": r.PathValue("id"), "revisions": revs})
}

func (g *Gateway) getGraphRevision(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "revision must be a number"})
		return
	}
	ir, err := g.St.GraphRevision(r.PathValue("id"), n)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(ir)
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
	g.ServeSkill(&wsWire{conn: conn, writer: writer}, PrincipalOf(r))
}

// ServeSkillOver serves a skill connection that did not arrive through the HTTP
// middleware, authenticating it here instead.
//
// WebTransport is the caller. Its `/ws/skill` route is served by a separate QUIC
// listener (internal/wtsrv) whose handlers never passed through
// Auth.Authenticate — so until this existed, a skill reaching the node over UDP
// registered with no credential at all, on a node whose TCP path required one.
// That is the kind of gap a second transport introduces quietly, and it made
// every scope decision below it decorative: a caller refused on TCP could
// present nothing on QUIC and be served.
//
// Authenticating here rather than inside wtsrv keeps one resolver and one scope
// table for both transports, which is the only arrangement in which the two
// cannot drift apart.
func (g *Gateway) ServeSkillOver(conn wire, r *http.Request) {
	p, ok := g.authenticateWire(r)
	if !ok {
		raw, _ := json.Marshal(channel.Envelope{
			V: channel.ProtocolMajor, ID: channel.NewID(), Kind: channel.KindError,
			Payload: json.RawMessage(
				`{"state":"error","detail":"this connection presented no credential this node accepts"}`),
		})
		_ = conn.Send(raw, channel.QoSReliable)
		return
	}
	g.ServeSkill(conn, p)
}

// authenticateWire resolves a principal for a non-HTTP-middleware connection.
// A node with authentication disabled has no principals to tell apart, so every
// caller is the operator — the same rule Authenticate applies.
func (g *Gateway) authenticateWire(r *http.Request) (Principal, bool) {
	if g.Auth == nil || g.Auth.Token == "" {
		return Principal{Scope: ScopeOperator}, true
	}
	p, err := g.Auth.resolve(r)
	if err != nil {
		return Principal{}, false
	}
	return p, p.Allows(http.MethodGet, "/ws/skill")
}

// wire is a transport as the session loops need it: whole envelopes in, whole
// envelopes out, tagged with the QoS the edge declared.
//
// It exists so that WebSocket and WebTransport share one registration
// handshake, one admission path and one emission loop rather than two that
// drift. The differences between the transports are real but they are all
// below this line — framing, keepalive, and which QUIC primitive a QoS class
// maps to (see internal/wtsrv).
type wire interface {
	Recv() ([]byte, error)
	Send(raw []byte, qos string) error
}

// wsWire adapts a gorilla connection, keeping the read deadline the ping/pong
// keepalive depends on.
type wsWire struct {
	conn   *websocket.Conn
	writer *wsWriter
}

func (w *wsWire) Recv() ([]byte, error) {
	_, raw, err := w.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	_ = w.conn.SetReadDeadline(time.Now().Add(pongWait))
	return raw, nil
}

func (w *wsWire) Send(raw []byte, qos string) error { return w.writer.Send(raw, qos) }

// ServeSkill runs the skill side of a connection to completion, whatever
// carried it. Exported so the WebTransport listener can reach it.
func (g *Gateway) ServeSkill(conn wire, p Principal) {
	connID := channel.NewID()
	fail := func(cause, msg string) {
		payload, _ := json.Marshal(map[string]string{"state": "error", "detail": msg})
		env := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
			CauseID: cause, Kind: channel.KindError, Payload: payload}
		raw, _ := json.Marshal(env)
		_ = conn.Send(raw, channel.QoSReliable)
	}

	// Registration handshake.
	first, err := conn.Recv()
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

	// The binding that makes a narrow credential narrow.
	//
	// Registration is how a process declares what it *is* to the executor, and
	// everything downstream keys off that: which policy rule applies, whether an
	// edge into it is an effect, which receipts it can spend at the broker. A
	// token scoped to one capability that could still register as another would
	// be a full token with a smaller name.
	if !p.MayRegister(manifest.Capability) {
		fail(regEnv.ID, fmt.Sprintf(
			"this credential may register %q and this manifest declares %q — "+
				"a scoped token registers as the capability it was issued for, and no other",
			p.Capability, manifest.Capability))
		return
	}

	// R15 admission: reserve the declared memory or explain the refusal.
	declaredMem, _ := manifest.Requirements["memory"].(string)
	if err := g.Adm.Admit(connID, manifest.ID, declaredMem); err != nil {
		fail(regEnv.ID, err.Error())
		return
	}
	defer g.Adm.Release(connID)

	live := &registry.Live{Manifest: manifest, Connected: time.Now(), Send: conn.Send}
	instances := g.Reg.Register(connID, live)
	defer g.Reg.Unregister(connID)
	if err := g.St.UpsertSkill(manifest.ID, manifest.Version, manifest); err != nil {
		g.Log.Error("persist skill failed", "err", err)
	}
	g.Log.Info("skill registered", "skill", manifest.ID, "type", manifest.Type,
		"capability", manifest.Capability)
	if instances > 1 {
		// Said at the moment it becomes true, because that is the only moment
		// anyone can act on it: a skill that outlived an earlier node may
		// reconnect long after this one finished starting up, and a check that
		// ran at startup would have found nothing to report.
		g.Log.Warn("skill has more than one connection",
			"skill", manifest.ID, "instances", instances,
			"note", "deliberate for replicas; otherwise a process outlived an earlier node")
	}

	effective, err := g.effectiveConfig(manifest)
	if err != nil {
		g.Log.Error("load skill config failed", "skill", manifest.ID, "err", err)
		effective = map[string]any{}
	}
	ack, _ := json.Marshal(map[string]any{
		"state": "registered", "skill": manifest.ID, "config": effective,
		// The decoding grammar for each of this skill's egress ports, derived
		// from the schema the port declares (C1). A skill that generates with
		// a model constrains it to this and then *cannot* emit something the
		// port would reject — see kernel/internal/grammar.
		//
		// Pushed at registration rather than fetched, because a skill needs it
		// before its first delivery and an extra round trip at startup is the
		// kind of friction that means nobody uses the feature.
		"grammars": g.egressGrammars(manifest),
	})
	ackEnv := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
		CauseID: regEnv.ID, Kind: channel.KindStatus, Payload: ack}
	rawAck, _ := json.Marshal(ackEnv)
	_ = conn.Send(rawAck, channel.QoSReliable)

	// Emission loop: everything the skill emits is dispatched to its session.
	for {
		raw, err := conn.Recv()
		if err != nil {
			g.Log.Info("skill disconnected", "skill", manifest.ID, "err", err)
			return
		}
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
	// undoOf marks this as an ephemeral undo session (Phase 2, `aura undo`):
	// the receipt of the effect the caller's one-edge graph is meant to
	// reverse. Empty for every ordinary connection.
	undoOf := r.URL.Query().Get("undo")

	up := g.upgrader()
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	writer := newWSWriter(conn)
	defer writer.Close()
	defer keepAlive(conn, writer)()

	sess, err := g.Mgr.Start(sessionID, graphID, undoOf, writer.Send)
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
	// Once data has flowed, the pin set is closed. See applyPins.
	pinned := false
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
		if handled, err := g.applyPins(sess, env, &pinned); handled {
			if err != nil {
				payload, _ := json.Marshal(map[string]string{"state": "error", "detail": err.Error()})
				e := channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
					Session: sessionID, Kind: channel.KindError, Payload: payload}
				rr, _ := json.Marshal(e)
				_ = writer.Send(rr, channel.QoSReliable)
			}
			continue
		}
		if env.Kind == channel.KindData {
			// Everything from here on runs against a fixed set of pins. See
			// applyPins for why that has to be true.
			pinned = true
		}
		sess.Route(env)
	}
}

// PinSchema marks the frame that fixes a session's pinned outputs.
const PinSchema = "aura/pins@1"

// applyPins handles a client frame that pins node outputs.
//
// Carried on `config_update`, which C3 already has, rather than on a new
// envelope kind: pinning is a property of this session's configuration and the
// contract is frozen for good reasons. Reported as handled either way, so a
// pin frame never reaches the graph as data.
//
// **Only before the first data envelope.** Changing what a node produces
// halfway through a session would make the causal log describe two different
// graphs under one session id, and anyone reading it afterwards would have no
// way to tell which half they were looking at.
func (g *Gateway) applyPins(sess *executor.Session, env channel.Envelope, pinned *bool) (bool, error) {
	if env.Kind != channel.KindConfigUpdate || env.Schema != PinSchema {
		return false, nil
	}
	if *pinned {
		return true, fmt.Errorf(
			"pins must be set before the first message: changing them mid-session would " +
				"leave one session id describing two different graphs")
	}
	var body struct {
		Pins map[string]executor.Pin `json:"pins"`
	}
	if err := json.Unmarshal(env.Payload, &body); err != nil {
		return true, fmt.Errorf("invalid pins: %w", err)
	}
	if err := sess.SetPins(body.Pins, string(g.Node.Mode)); err != nil {
		return true, err
	}
	g.Log.Info("session pins set", "session", sess.ID, "nodes", len(body.Pins))
	return true, nil
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
