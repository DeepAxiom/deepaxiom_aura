package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/executor"
)

// GET /v1/watch?session=<id>[&history=<n>][&ticket=<t>] — attach to a live
// session as a read-only observer.
//
// The socket receives every envelope the session delivers to its client, from
// the moment it attaches, and that is all it does: frames it sends are read
// and discarded, a gate's confirm_request is never shown to it, and it cannot
// slow the session — a watcher that falls behind is disconnected with a
// status saying so, rather than made to wait or handed a stream with holes.
// See executor/watch.go for why each of those is the rule.
//
// # Who may watch
//
// The operator, or the bearer of a watch ticket for this session — which is
// the credential an application gives a viewer so the viewer can connect to
// the node directly. See ticket.go. A ticket's session is the only one it
// opens: asking for another with it is refused, not redirected.
//
// # History
//
// Someone who joins a stream in the middle of a sentence is looking at half a
// sentence. `history=<n>` replays, before anything live, the last n envelopes
// the session delivered to its client — read from the causal log, so it is
// exactly what the client was sent — bracketed by two statuses so a viewer
// can tell replay from live:
//
//	{"state":"history","count":k}   k envelopes follow, oldest first
//	...k envelopes...
//	{"state":"watching"}            everything after this is live
//
// The watcher is attached *before* the log is read, and what arrives live in
// the meantime is held and then de-duplicated against the replay by envelope
// id. So there is no gap between the two and nothing is delivered twice. Capped
// at 500: this is a way to catch up on a conversation, not an export — the
// export is `GET /v1/sessions/{id}/events`.
//
// Payloads the log elided (a `realtime` edge records that a frame existed and
// not its bytes) are not replayed: an audio frame nobody kept cannot be sent.
func (g *Gateway) watchWS(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	// A presented ticket binds the connection to the session it names, whoever
	// else the caller might be. Verified here as well as in the auth
	// middleware because a node running with --no-auth has no middleware
	// decision to inherit: there, every caller is the operator, and a ticket
	// must still mean what it says rather than being ignored.
	if ticket := r.URL.Query().Get("ticket"); ticket != "" {
		token := ""
		if g.Auth != nil {
			token = g.Auth.Token
		}
		claims, err := verifyTicket(g.ticketKeys().get(token), ticket, time.Now())
		if err != nil {
			http.Error(w, errBadTicket.Error(), http.StatusUnauthorized)
			return
		}
		if sessionID == "" {
			sessionID = claims.Session
		}
		if sessionID != claims.Session {
			http.Error(w, "this ticket is for another session", http.StatusForbidden)
			return
		}
	} else if PrincipalOf(r).Scope == ScopeWatch {
		// Unreachable today — a watch principal only ever comes from a ticket —
		// and refused rather than trusted if that ever stops being true.
		http.Error(w, errBadTicket.Error(), http.StatusUnauthorized)
		return
	}
	if sessionID == "" {
		http.Error(w, "missing ?session=<session_id>", http.StatusBadRequest)
		return
	}
	history := 0
	if raw := r.URL.Query().Get("history"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			http.Error(w, "history must be a non-negative integer", http.StatusBadRequest)
			return
		}
		if n > maxWatchHistory {
			n = maxWatchHistory
		}
		history = n
	}
	// Refused before the upgrade, so a client asking for a session that is
	// not live gets an HTTP status it can read rather than a socket that
	// opens and immediately closes.
	sess, ok := g.Mgr.Get(sessionID)
	if !ok {
		http.Error(w, "no live session "+sessionID, http.StatusNotFound)
		return
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

	status := func(fields map[string]any) []byte {
		fields["session"] = sessionID
		payload, _ := json.Marshal(fields)
		raw, _ := json.Marshal(channel.Envelope{V: channel.ProtocolMajor, ID: channel.NewID(),
			Session: sessionID, Kind: channel.KindStatus, Payload: payload})
		return raw
	}

	// Live frames are held here until the replay has gone out, then passed
	// straight through.
	gate := &watchGate{out: writer.TrySend}

	cancel, err := sess.Watch(gate.offer, func(reason string) {
		// Called from the session's routing path: nothing here may block.
		// The farewell is best-effort — a watcher cut off for being behind
		// has, by definition, no room for it. The close itself is handed to
		// another goroutine because closing the writer waits for it to drain
		// what is queued, farewell included, and only then is the connection
		// closed, which is what ends the read loop below.
		writer.TrySend(status(map[string]any{"state": "ended", "detail": reason}))
		go func() {
			writer.Close()
			_ = conn.Close()
		}()
	})
	if err != nil {
		_ = writer.Send(status(map[string]any{"state": "error", "detail": err.Error()}),
			channel.QoSReliable)
		return
	}
	defer cancel()

	replayed := map[string]bool{}
	if history > 0 {
		past := g.clientHistory(sess, sessionID, history)
		_ = writer.Send(status(map[string]any{"state": "history", "count": len(past)}),
			channel.QoSReliable)
		for _, item := range past {
			replayed[item.id] = true
			if err := writer.Send(item.raw, channel.QoSReliable); err != nil {
				return
			}
		}
	}
	_ = writer.Send(status(map[string]any{"state": "watching"}), channel.QoSReliable)
	if !gate.open(replayed) {
		// More arrived during the replay than a watcher is allowed to be
		// behind. Same rule as any other time: cut off, not skipped.
		_ = writer.Send(status(map[string]any{"state": "ended",
			"detail": "fell behind the session during replay; reconnect to resume watching"}),
			channel.QoSReliable)
		return
	}

	// Read-only. Frames are consumed so control frames (ping, close) are
	// processed, and their contents go nowhere.
	pong := pongWait // this connection's deadline window, read once; see keepAlive
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(pong))
	}
}

// maxWatchHistory bounds a replay, and maxWatchPending bounds what may arrive
// live while one is being sent.
const (
	maxWatchHistory = 500
	maxWatchPending = 256
)

type pastFrame struct {
	id  string
	raw []byte
}

// clientHistory returns the last n envelopes the session delivered to its
// client, oldest first, from the causal log.
func (g *Gateway) clientHistory(sess *executor.Session, sessionID string, n int) []pastFrame {
	// The tail of the log, not its head: on a session that has been live for
	// hours, the first twenty thousand events are the morning.
	events, err := g.St.SessionEventsTail(sessionID, watchHistoryScan)
	if err != nil {
		return nil
	}
	ports := sess.ClientPorts()
	var out []pastFrame
	for _, raw := range events {
		var env channel.Envelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		// What the kernel sent the client: logged under the client node, on a
		// port the graph delivers to, and never a gate — that is for the
		// client alone, in replay exactly as live.
		if env.Node != executor.ClientRef || !ports[env.Port] ||
			env.Kind == channel.KindConfirmRequest || env.Kind == channel.KindConfirmResponse {
			continue
		}
		if elidedPayload(env.Payload) {
			continue
		}
		out = append(out, pastFrame{id: env.ID, raw: raw})
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// watchHistoryScan is how much of a session's log a replay reads to find its
// last n client-bound envelopes.
const watchHistoryScan = 20000

func elidedPayload(payload json.RawMessage) bool {
	if len(payload) == 0 {
		return false
	}
	var probe struct {
		Elided bool `json:"elided"`
	}
	return json.Unmarshal(payload, &probe) == nil && probe.Elided
}

// watchGate holds live frames while a replay is being sent, then lets them
// through.
type watchGate struct {
	mu      sync.Mutex
	out     func([]byte) bool
	opened  bool
	pending [][]byte
	lost    bool
}

// offer is the watcher's non-blocking sink. Before the gate opens it only
// appends to a bounded buffer; after, it is the writer's own TrySend.
func (g *watchGate) offer(raw []byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.opened {
		return g.out(raw)
	}
	if len(g.pending) >= maxWatchPending {
		g.lost = true
		return false
	}
	g.pending = append(g.pending, raw)
	return true
}

// open flushes what was held — minus anything the replay already sent — and
// switches to passing frames straight through. False if the watcher fell
// behind while it was held.
func (g *watchGate) open(replayed map[string]bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost {
		return false
	}
	for _, raw := range g.pending {
		if len(replayed) > 0 {
			var head struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &head) == nil && replayed[head.ID] {
				continue
			}
		}
		if !g.out(raw) {
			return false
		}
	}
	g.pending = nil
	g.opened = true
	return true
}
