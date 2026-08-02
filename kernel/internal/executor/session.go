package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// dest is a wired destination of an edge.
type dest struct {
	ref   string // node ref in the graph (or "client")
	port  string // ingress port on the destination
	gate  string // "" or "human-approval"
	skill *registry.Live
	// schema declared by the destination ingress port ("" = untyped/client)
	schema string
	// qos is the edge's declared delivery class (C3 rule 4): whether a slow
	// receiver blocks the producer or loses the oldest frames.
	qos string
	// speculative: deliver this edge on partial upstream output and discard
	// the work if the final output differs (C2 v1.2, see speculation.go).
	// Never true for a motor destination — applyPolicy refuses it there.
	speculative bool
	// deadlineMS: how long a delivery here is worth waiting for. Rides on the
	// envelope so the receiver can degrade rather than arrive late.
	deadlineMS int
	// priority orders preemption between contending chains. Higher wins.
	priority int
	// from is the producing endpoint ("ref.port"), needed to key speculation
	// state per stream rather than per destination.
	from string
}

// Session is a live instance of a graph (P4). Single-writer: owned by this node.
type Session struct {
	ID    string
	Graph *Graph

	routes map[string][]dest // "ref.port" -> destinations
	seq    map[string]uint64 // "ref.port" -> forward sequence counter
	seqMu  sync.Mutex

	dedup *channel.Dedup
	st    *store.Store
	log   *slog.Logger
	// policy is the node's authorization document. Held past wiring because a
	// rule's rate limit can only be answered per delivery — see forward.
	policy *Policy
	// ldg is the node's effect ledger (C4). Nil is a valid, deliberate state —
	// see sealEffect — so tests that only exercise routing never have to wire
	// a store and a keypair just to construct a Session.
	ldg *ledger.Ledger
	// undoOf is non-empty only for an ephemeral undo session (Phase 2, `aura
	// undo`): the receipt of the effect this session's single edge reverses.
	// NewSession validated it against the ledger before this Session was ever
	// built — see validateUndo — so sealEffect only has to attach it.
	undoOf string

	// pending human-approval gates: confirm_request id -> held envelope + dest
	pendMu  sync.Mutex
	pending map[string]pendingGate

	// Cancel bookkeeping — see causal.go. Together these let a cancel naming
	// one client message reach every skill working on it, at any depth, and
	// let the kernel suppress whatever those skills emit afterwards.
	causal    *causalIndex
	inFlight  *inFlightIndex
	cancelled *cancelledSet
	// attests binds C5 inference attestations to the causal chain they were
	// produced in, so an effect can be sealed citing what argued for it.
	attests *attestIndex
	// spec tracks speculative deliveries per stream (C2 v1.2). See
	// speculation.go.
	spec *specIndex
	// ctxLedger accounts this session against the graph's declared
	// context_budget (C2 v1.2). Nil when the graph declared none.
	ctxLedger *contextLedger

	sendClient func(raw []byte, qos string) error
}

type pendingGate struct {
	env channel.Envelope
	to  dest
}

// applyPolicy enforces the kernel's safety invariant: an edge that delivers
// into a skill acting on the world carries whatever gate the *node* has
// decided on — not whatever the graph asked for.
//
// This lives here, in the single place every graph passes through, rather than
// in each producer of graphs. Before, the rule was implemented twice in
// userland (skills/planner and the MCP server) and the executor only enforced
// gates already written into the IR, so a hand-declared graph could reach a
// motor skill with no gate at all and the write would go through unapproved. A
// visual graph editor would have been a third implementation of the same rule,
// and a declarative connector a fourth.
//
// Moving the *decision* out of the graph and into a node policy closes the
// remaining hole. An edge saying `"gate": "none"` used to be the last word,
// and a graph is just a JSON document someone POSTs — so the guarantee held
// against a careless author and not against a hostile one. Now the waiver is a
// request the node grants or ignores; see policy.go for the ordering rule.
//
// Two things published mode still does on its own, because they are about
// telling an author they made a mistake rather than about authorization:
// an *omitted* gate on a motor edge is refused outright rather than quietly
// repaired, and a graph-level waiver is never honoured there whatever the
// policy says — on a public network the node is the only authority.
func applyPolicy(d *dest, pol *Policy, mode string, from, to string) error {
	if d.skill == nil {
		return nil // the client pseudo-node acts on nothing
	}
	published := mode == string(identity.ModePublished)

	// The speculation invariant, stated here because it is the motor gate seen
	// from another angle: the type of skill that must wait for a human is the
	// type that must never run ahead of certainty.
	//
	// C1's five types are an effect type system, so "is this safe to run
	// early?" is answered by the manifest rather than by the graph author's
	// judgement or a heuristic. Every other runtime that speculates has to
	// decide this per tool, by hand, and gets it wrong when someone adds a
	// tool that writes.
	//
	// Refused rather than ignored: an author who wrote `speculative: true` on
	// an edge that sends an email has misunderstood something, and silently
	// dropping the flag would leave them believing it worked.
	if d.speculative && d.skill.Manifest.Type == TypeMotor {
		return fmt.Errorf(
			"edge %s -> %s declares speculative but %q is a motor skill; work that acts "+
				"on the world is never run ahead of certainty, because a discarded effect "+
				"is not discarded", from, to, d.skill.Manifest.ID)
	}
	if d.speculative && !pol.SpeculationAllowed() {
		// A node-wide opt-out is a preference, not a mistake, so the edge is
		// downgraded rather than refused. The counter records it.
		d.speculative = false
	}

	if published && d.gate == "" && d.skill.Manifest.Type == TypeMotor {
		return fmt.Errorf("edge %s -> %s delivers into motor skill %q with no gate; "+
			"published mode requires an explicit %q gate on edges that act on the world",
			from, to, d.skill.Manifest.ID, GateHumanApproval)
	}

	graphGate := d.gate
	if published && graphGate == GateNone {
		// Drop the waiver before asking: in published mode the graph does not
		// get a vote, so policy decides as if the edge had said nothing.
		graphGate = ""
	}

	gate, err := pol.Authorize(d.skill.Manifest.Capability, graphGate)
	if err != nil {
		return fmt.Errorf("edge %s -> %s: %w", from, to, err)
	}
	d.gate = gate
	return nil
}

// NewSession resolves every node against the live registry, validates edge
// schema compatibility (C2 rule 2), applies the node's authorization policy,
// and builds the routing table. mode is the node's security mode (C1
// progressive security); pol is the policy document in force — pass
// DefaultPolicy() for the pre-policy behaviour. ldg is the node's effect
// ledger (C4); a production node always supplies one — nil is accepted only
// so tests that do not care about attestation are not forced to construct a
// store and a signing keypair just to build a Session. undoOf is empty for
// an ordinary session; for an ephemeral undo session (Phase 2, `aura undo`)
// it is the receipt of the effect g's one edge is meant to reverse, and is
// validated against the ledger here — before the session exists — the same
// "refuse before building" pattern a policy deny already uses (see
// validateUndo).
func NewSession(id string, g *Graph, reg *registry.Registry, st *store.Store,
	mode string, pol *Policy, ldg *ledger.Ledger, undoOf string,
	sendClient func(raw []byte, qos string) error, log *slog.Logger) (*Session, error) {

	if pol == nil {
		pol = DefaultPolicy()
	}

	resolved := map[string]*registry.Live{}
	for _, n := range g.Nodes {
		// C4 policy `routes`: which of several providers answers a capability
		// is a governance decision, readable in the same signed document that
		// says what may act on the world.
		prefer, avoid := pol.RouteFor(n.Resolve)
		live, err := reg.ResolvePreferred(n.Use, n.Resolve, prefer, avoid)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.Ref, err)
		}
		resolved[n.Ref] = live
	}

	if undoOf != "" {
		if err := validateUndo(st, undoOf, g, resolved); err != nil {
			return nil, err
		}
	}

	s := &Session{
		ID: id, Graph: g,
		routes:     map[string][]dest{},
		seq:        map[string]uint64{},
		dedup:      channel.NewDedup(4096),
		st:         st,
		policy:     pol,
		ldg:        ldg,
		undoOf:     undoOf,
		log:        log,
		pending:    map[string]pendingGate{},
		causal:     newCausalIndex(maxCausalRoots),
		inFlight:   newInFlightIndex(maxInFlightRoot),
		cancelled:  newCancelledSet(maxCancelled),
		attests:    newAttestIndex(maxAttestRoots, maxAttestPerRoot),
		spec:       newSpecIndex(maxSpecRoots),
		ctxLedger:  newContextLedger(g.ContextBudget),
		sendClient: sendClient,
	}

	for _, e := range g.Edges {
		fromRef, fromPort := splitEndpoint(e.From)
		toRef, toPort := splitEndpoint(e.To)

		qos := e.QoS
		if qos == "" {
			qos = channel.QoSReliable // C3 rule 4: reliable is the default
		}
		d := dest{
			ref: toRef, port: toPort, gate: e.Gate, qos: qos, from: e.From,
			speculative: e.Speculative, deadlineMS: e.DeadlineMS, priority: e.Priority,
		}
		if toRef != ClientRef {
			live := resolved[toRef]
			sch, ok := live.Manifest.IngressSchema(toPort)
			if !ok {
				return nil, fmt.Errorf("edge %s -> %s: %q has no ingress port %q",
					e.From, e.To, live.Manifest.ID, toPort)
			}
			d.skill = live
			d.schema = sch
			// C2 rule 2: schema compatibility when both sides are declared.
			if fromRef != ClientRef {
				if fromSch, ok := resolved[fromRef].Manifest.EgressSchema(fromPort); ok && fromSch != sch {
					return nil, fmt.Errorf("edge %s -> %s: schema mismatch %q vs %q",
						e.From, e.To, fromSch, sch)
				}
			}
			if err := applyPolicy(&d, pol, mode, e.From, e.To); err != nil {
				return nil, err
			}
		}
		s.routes[e.From] = append(s.routes[e.From], d)
	}

	if err := s.resumeFromLog(); err != nil {
		return nil, fmt.Errorf("resume session %q: %w", id, err)
	}
	return s, nil
}

// validateUndo refuses to build an undo session unless the ledger agrees the
// undo makes sense — before any envelope moves, mirroring how a policy deny
// is resolved before a session exists rather than on first delivery (C4,
// "What deny does"). g is the caller's ephemeral one-edge graph
// (client.<port> -> skill.<compensates.port>); resolved is what NewSession
// already resolved that edge's target against the live registry.
func validateUndo(st *store.Store, undoOf string, g *Graph, resolved map[string]*registry.Live) error {
	raw, err := st.LedgerEntryByHash(undoOf)
	if err != nil {
		return fmt.Errorf("undo %s: %w", undoOf, err)
	}
	var entry ledger.Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fmt.Errorf("undo %s: stored entry is corrupt: %w", undoOf, err)
	}
	if entry.Outcome != spec.OutcomeDelivered {
		return fmt.Errorf("undo %s: refused — effect was never delivered (outcome=%q)", undoOf, entry.Outcome)
	}
	if entry.Compensation == nil {
		return fmt.Errorf("undo %s: refused — the skill that produced this effect declared no compensation", undoOf)
	}
	if _, found, err := st.LedgerFindByCompensates(undoOf); err != nil {
		return fmt.Errorf("undo %s: %w", undoOf, err)
	} else if found {
		return fmt.Errorf("undo %s: refused — this effect was already undone", undoOf)
	}
	if len(g.Edges) != 1 {
		return fmt.Errorf("undo %s: an undo graph must have exactly one edge, got %d", undoOf, len(g.Edges))
	}
	toRef, toPort := splitEndpoint(g.Edges[0].To)
	live, ok := resolved[toRef]
	if !ok {
		return fmt.Errorf("undo %s: edge target %q did not resolve", undoOf, toRef)
	}
	if live.Manifest.Capability != entry.Compensation.Capability {
		return fmt.Errorf("undo %s: edge targets capability %q, but the declared compensation capability is %q",
			undoOf, live.Manifest.Capability, entry.Compensation.Capability)
	}
	if toPort != entry.Compensation.Port {
		return fmt.Errorf("undo %s: edge targets port %q, but the declared compensation port is %q",
			undoOf, toPort, entry.Compensation.Port)
	}
	return nil
}

func splitEndpoint(ep string) (ref, port string) {
	parts := strings.SplitN(ep, ".", 2)
	return parts[0], parts[1]
}

// rootOf resolves which client message an envelope's causal chain started
// from. A client-originated envelope is its own root; anything a skill emits
// inherits the root of whatever the kernel delivered to it.
func (s *Session) rootOf(env channel.Envelope) string {
	if env.Node == ClientRef {
		return env.ID
	}
	if root, ok := s.causal.get(env.CauseID); ok {
		return root
	}
	return ""
}

// Route ingests an envelope emitted by a source (client or skill), logs it,
// and forwards a new causally-linked envelope per matching edge (C3).
func (s *Session) Route(env channel.Envelope) {
	raw, _ := json.Marshal(env)
	if err := s.st.AppendEvent(s.ID, env.ID, env.CauseID, env.Kind, raw); err != nil {
		s.log.Error("event log append failed", "err", err)
	}
	if env.Kind == channel.KindCancel {
		s.handleCancel(env)
		return
	}
	if env.Kind == channel.KindData && s.dedup.Seen(env.Idem) {
		return // at-least-once: duplicate delivery, already processed
	}
	if env.Kind == channel.KindConfirmResponse {
		s.resolveGate(env)
		return
	}

	// Suppression is what makes cancel a guarantee rather than a request.
	// A skill may ignore `cancel`, or notice it only at its next checkpoint;
	// either way nothing it emits afterwards travels any further. Deliberately
	// after AppendEvent: the log must stay a truthful record of what the skill
	// actually did, so `aura why` can show work that was cancelled.
	root := s.rootOf(env)
	if s.cancelled.has(root) {
		s.log.Debug("suppressed post-cancel envelope",
			"session", s.ID, "root", root, "id", env.ID, "kind", env.Kind)
		return
	}

	// C5: bind whatever this skill claims about how it produced this output
	// to the chain, so an effect sealed downstream can cite it. Done after
	// the suppression check — a cancelled chain's attestations are already in
	// the event log and binding them to nothing serves no purpose.
	s.recordAttestation(root, env)

	key := env.Node + "." + env.Port
	dests := s.routes[key]
	if len(dests) == 0 {
		s.log.Debug("no route", "session", s.ID, "from", key, "kind", env.Kind)
		return
	}
	for _, d := range dests {
		if d.speculative {
			s.forwardSpeculatively(env, d, root)
			continue
		}
		if d.gate == GateHumanApproval {
			s.holdForApproval(env, d)
			continue
		}
		s.forward(env, d)
	}
}

// forwardSpeculatively implements C2 v1.2 speculation on one edge.
//
// The producer streams partials; this folds them into a running value and
// hands that value downstream *as if it were final*, then reconciles when the
// true final arrives. See speculation.go for why the effect type system is
// what makes this safe rather than reckless.
func (s *Session) forwardSpeculatively(env channel.Envelope, d dest, root string) {
	if env.Kind != channel.KindData || root == "" {
		// Terminal and control envelopes are not speculation material: a
		// `done` or an `error` is the tail of work, not a guess about it.
		s.forward(env, d)
		return
	}

	key := specKey{root: root, from: d.from}
	running := s.spec.fold(key, env.Schema, env.Payload)

	if !isFinal(env.Payload) {
		// A partial. Deliver the accumulated value *marked final*, because the
		// whole point is that the downstream treats it as a complete input and
		// starts real work — a consumer handed `final: false` would wait, and
		// waiting is what speculation exists to avoid.
		guess := env
		guess.Payload = markFinal(running)
		s.forward(guess, d)
		s.spec.speculate(key, guess.ID)
		return
	}

	// The real end of the stream. Either the guess stands or it does not.
	hit, stale := s.spec.resolve(key, running)
	if hit {
		// The downstream already has this exact value and has been working on
		// it. Delivering it again would duplicate the work speculation just
		// saved, and — for an idempotent consumer — produce a second reply.
		s.log.Debug("speculation hit; suppressing redundant final delivery",
			"session", s.ID, "edge", d.from+" -> "+d.ref+"."+d.port)
		return
	}

	// A miss. Abandon what the speculative deliveries started, using the same
	// suppression `cancel` uses, then deliver the truth.
	for _, envelopeID := range stale {
		s.abandonSpeculation(envelopeID, d)
	}
	real := env
	real.Payload = running
	s.forward(real, d)
}

// abandonSpeculation stops work started on a guess that turned out wrong.
//
// It reuses the cancel path rather than inventing a second one: the guarantee
// that a cancelled chain's output goes nowhere is exactly the guarantee a
// speculative miss needs, and having one mechanism means a skill that handles
// cancel correctly handles this correctly for free.
func (s *Session) abandonSpeculation(envelopeID string, d dest) {
	s.cancelled.add(envelopeID)
	if d.skill == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"reason": "speculation superseded: the producer's final output differs from the " +
			"partial this work was started on",
	})
	cancel := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: envelopeID,
		Session: s.ID, Node: d.ref, Port: d.port, Kind: channel.KindCancel,
		Payload: payload,
	}
	raw, _ := json.Marshal(cancel)
	_ = s.st.AppendEvent(s.ID, cancel.ID, cancel.CauseID, cancel.Kind, raw)
	if err := d.skill.Send(raw, channel.QoSReliable); err != nil {
		s.log.Debug("could not ask a skill to abandon speculative work",
			"session", s.ID, "to", d.ref+"."+d.port, "err", err)
	}
}

// SpeculationStats reports this session's guessing record.
func (s *Session) SpeculationStats() SpeculationStats { return s.spec.Stats() }

// handleCancel implements the C3 "cancel" kind (spec/c3-channel.md).
//
// The client names the message it wants abandoned. Two things happen:
//
//  1. The chain is marked cancelled, so anything still arriving for it is
//     suppressed in Route. This is the part that holds even against a skill
//     that ignores `cancel` entirely.
//  2. Every skill known to be working on that chain is asked to stop — each
//     addressed with the cause_id *it* will recognise, which is the whole
//     reason the kernel keeps a causal index. A cancel carrying the client's
//     own id would mean nothing to a skill three hops downstream.
//
// Asking is still best-effort: a skill decides when to notice.
func (s *Session) handleCancel(env channel.Envelope) {
	root := env.CauseID
	if r, ok := s.causal.get(env.CauseID); ok {
		root = r // the client named a mid-chain envelope; cancel the whole chain
	}
	if root == "" {
		return
	}
	s.cancelled.add(root)

	hops := s.inFlight.take(root)
	s.log.Debug("cancel propagated", "session", s.ID, "root", root, "skills", len(hops))
	for _, h := range hops {
		cancelEnv := channel.Envelope{
			V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: h.causeID,
			Session: s.ID, Node: h.to.ref, Port: h.to.port, Kind: channel.KindCancel,
		}
		raw, _ := json.Marshal(cancelEnv)
		if err := h.to.skill.Send(raw, channel.QoSReliable); err != nil {
			s.log.Debug("cancel delivery failed", "to", h.to.ref, "err", err)
		}
	}
}

// forward emits a new envelope for the edge hop: new id, cause = source id,
// deterministic idem so kernel retries deduplicate downstream.
func (s *Session) forward(src channel.Envelope, d dest) {
	// The dynamic half of the policy check. Only `data` counts against a rate
	// limit: a `done` or an `error` is the tail of work already authorized, and
	// refusing to deliver one would leave the consumer waiting forever for
	// something that already happened.
	if d.skill != nil && src.Kind == channel.KindData {
		if err := s.policy.CheckRate(d.skill.Manifest.Capability); err != nil {
			s.log.Warn("policy refused delivery", "session", s.ID,
				"to", d.ref+"."+d.port, "err", err)
			s.emitError(src.ID, err.Error())
			return
		}
	}

	hopKey := d.ref + "." + d.port
	s.seqMu.Lock()
	s.seq[hopKey]++
	seq := s.seq[hopKey]
	s.seqMu.Unlock()

	out := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: src.ID,
		Session: s.ID, Node: d.ref, Port: d.port, Seq: seq,
		Idem:   fmt.Sprintf("%s:%s:%s", src.Idem, d.ref, d.port),
		Schema: src.Schema, Kind: src.Kind, Payload: src.Payload,
		Priority: d.priority, Speculative: d.speculative,
	}
	// C3 v1.6: an absolute instant, computed once at the hop that declares it
	// and then *inherited* rather than recomputed. A budget that restarted at
	// every hop would let a three-hop chain quietly spend three times what its
	// author allowed.
	out.Deadline = inheritDeadline(src.Deadline, d.deadlineMS)

	// C2 v1.2: refuse to deliver into a chain that has already blown the
	// graph's context budget, rather than letting a model discover it as a
	// truncation or an out-of-memory two hops later.
	if err := s.chargeContext(out); err != nil {
		s.log.Warn("context budget exceeded", "session", s.ID,
			"to", d.ref+"."+d.port, "err", err)
		s.emitError(src.ID, err.Error())
		return
	}

	// Keep the chain traceable back to the client message that started it, and
	// remember what this destination will call the work we are handing it.
	// `src.ID` is exactly the cause_id the destination sees on `out`, which is
	// what a cancel must carry to be recognised there.
	root := s.rootOf(src)
	if root != "" {
		s.causal.set(out.ID, root)
		s.inFlight.record(root, hop{to: d, causeID: src.ID})
	}

	// The Effect Checkpoint (C4): a data envelope landing in a motor skill is
	// an effect, and gets attested before it goes anywhere. Before marshaling
	// `raw`, so a successful seal's receipt travels both in the persisted
	// event log and on the envelope the skill actually receives.
	//
	// `root` is passed in rather than re-derived because the seal must cite
	// the inferences of *this* chain (C5), and the chain is what the root
	// names.
	s.sealEffect(&out, d, root)

	// C3 v1.6: a delivery whose deadline has already passed is dropped rather
	// than sent. The receiver would compute an answer nobody is waiting for,
	// and on a busy node that work displaces work that still matters.
	if s.dropExpired(out, d) {
		return
	}

	raw, _ := json.Marshal(out)
	if err := s.st.AppendEvent(s.ID, out.ID, out.CauseID, out.Kind, s.loggable(out, raw, d.qos)); err != nil {
		s.log.Error("event log append failed", "err", err)
	}

	// Only `data` rides the declared lane. A done, an error or a status is
	// terminal or explanatory: dropping one leaves a consumer waiting forever
	// for something that already happened.
	qos := d.qos
	if out.Kind != channel.KindData {
		qos = channel.QoSReliable
	}

	var err error
	if d.ref == ClientRef {
		err = s.sendClient(raw, qos)
	} else {
		err = d.skill.Send(raw, qos)
	}
	if err != nil {
		s.emitError(src.ID, fmt.Sprintf("delivery to %s.%s failed: %v", d.ref, d.port, err))
	}
}

// loggable is what gets persisted for an envelope.
//
// C3 rule 7 requires every envelope in the causal log; it does not require
// every byte of every payload. A minute of speech is roughly 12 MB of base64
// in SQLite and a permanent recording of someone talking, sitting in ~/.aura
// forever. Neither `aura why` nor `aura replay` needs the samples to do their
// job — they need the chain — so on a `realtime` edge the payload is replaced
// by a description of itself.
//
// Keyed on the edge's declared QoS rather than on the schema, deliberately: a
// graph that declares an audio edge `reliable` is asking for a recording, and
// gets one.
func (s *Session) loggable(env channel.Envelope, raw []byte, qos string) []byte {
	if qos != channel.QoSRealtime || len(env.Payload) == 0 {
		return raw
	}
	sum := sha256.Sum256(env.Payload)
	env.Payload, _ = json.Marshal(map[string]any{
		"elided": true,
		"bytes":  len(env.Payload),
		"sha256": hex.EncodeToString(sum[:8]),
	})
	elided, err := json.Marshal(env)
	if err != nil {
		return raw
	}
	return elided
}

// holdForApproval implements the human-approval gate (C2 rule 4).
func (s *Session) holdForApproval(env channel.Envelope, d dest) {
	reqID := channel.NewID()
	s.pendMu.Lock()
	s.pending[reqID] = pendingGate{env: env, to: d}
	s.pendMu.Unlock()

	payload, _ := json.Marshal(map[string]any{
		"question": fmt.Sprintf("Approve delivery to %s.%s?", d.ref, d.port),
		"options":  []string{"approve", "deny"},
		"held":     env.ID,
		// to_ref/to_port name the held delivery's destination (additive,
		// Phase 2). A human only needs "question" to answer, but resumeFromLog
		// needs these to unambiguously re-derive `dest` from the routing table
		// after a restart — the held envelope's source port alone is not
		// enough when a session has more than one human-approval gate reachable
		// from it.
		"to_ref":  d.ref,
		"to_port": d.port,
	})
	req := channel.Envelope{
		V: channel.ProtocolMajor, ID: reqID, CauseID: env.ID, Session: s.ID,
		Node: ClientRef, Port: "confirm_in", Kind: channel.KindConfirmRequest,
		Schema: "std/confirmation@1", Payload: payload,
	}
	raw, _ := json.Marshal(req)
	_ = s.st.AppendEvent(s.ID, req.ID, req.CauseID, req.Kind, raw)
	if err := s.sendClient(raw, channel.QoSReliable); err != nil {
		s.log.Error("confirm_request delivery failed", "err", err)
	}
}

func (s *Session) resolveGate(resp channel.Envelope) {
	s.pendMu.Lock()
	held, ok := s.pending[resp.CauseID]
	if ok {
		delete(s.pending, resp.CauseID)
	}
	s.pendMu.Unlock()
	if !ok {
		return
	}
	var body struct {
		Approve bool `json:"approve"`
	}
	_ = json.Unmarshal(resp.Payload, &body)
	raw, _ := json.Marshal(resp)
	_ = s.st.AppendEvent(s.ID, resp.ID, resp.CauseID, resp.Kind, raw)
	if body.Approve {
		s.forward(held.env, held.to)
	} else {
		s.sealDenial(held)
		s.emitError(resp.ID, fmt.Sprintf("delivery to %s.%s denied by user", held.to.ref, held.to.port))
	}
}

// recordAttestation captures a C5 inference attestation (envelope.Attest) and
// binds it to the causal chain it was produced in.
//
// Three properties are deliberate:
//
//   - **Malformed is dropped, not fatal.** A skill that sends a broken
//     attestation still gets its output routed. The alternative — refusing
//     the envelope — would let a metadata bug take down a working graph, and
//     the effect that follows will simply be sealed citing one fewer
//     inference. The log records the rejection so the gap is explicable.
//   - **Stored before it is bound.** The hash goes into the index only once
//     the record is durable, so a sealed entry can never cite an attestation
//     that was never written. A dangling hash in an immutable chain is
//     unfixable; a missing citation is merely incomplete.
//   - **Not restricted to cognitive skills.** An ASR skill's model matters to
//     an effect for exactly the same reason an LLM's does — a mis-transcribed
//     amount is as consequential as a mis-reasoned one.
func (s *Session) recordAttestation(root string, env channel.Envelope) {
	if len(env.Attest) == 0 || root == "" {
		return
	}
	_, hash, err := ledger.ParseAttestation(env.Attest)
	if err != nil {
		s.log.Warn("ignoring malformed inference attestation",
			"session", s.ID, "from", env.Node+"."+env.Port, "err", err)
		return
	}
	if err := ledger.RecordAttestation(s.st, hash, env.Attest); err != nil {
		s.log.Error("storing inference attestation failed",
			"session", s.ID, "hash", hash, "err", err)
		return
	}
	s.attests.add(root, hash)
}

// sealEffect: el paso de attest del Effect Checkpoint (C4). Ojo, no todo se
// sella — solo un envelope `data` que aterriza en un skill `motor.*` cuenta
// como efecto. Tokens de un cognitive, un status, un done: eso no es un
// efecto, es tráfico normal. Si sellara todo, el ledger dejaría de ser
// evidencia de efectos y pasaría a ser una copia barata del causal log que
// C3 rule 7 ya te da gratis.
//
// Sobre el error path: si Seal() falla (disco lleno, store roto), se loguea
// y la entrega sigue de todos modos — mismo trade-off que AppendEvent unas
// líneas arriba. La razón es simple: un fallo operacional al persistir no
// debería convertirse en un DoS contra cada efecto del nodo. Sí, eso deja un
// hueco documentado en el ledger en vez de tumbar el nodo — pero para algo
// que promete "seguir vivo" como premisa central, tumbar el nodo es el peor
// de los dos males.
func (s *Session) sealEffect(out *channel.Envelope, d dest, root string) {
	if s.ldg == nil || d.skill == nil || d.skill.Manifest.Type != TypeMotor || out.Kind != channel.KindData {
		return
	}
	decision := spec.DecisionAllow
	if d.gate == GateHumanApproval {
		decision = spec.DecisionGate
	}
	inference, truncated := s.attests.get(root)
	if truncated {
		// Say so rather than sealing a set that looks complete and is not.
		// An entry citing three of five inferences, with nothing recording
		// that two were dropped, is worse than one that cites none.
		s.log.Warn("inference attestations truncated for this chain; "+
			"the sealed entry cites a partial set",
			"session", s.ID, "root", root, "cited", len(inference))
	}
	req := ledger.SealRequest{
		Session: s.ID, Envelope: out.ID, Cause: out.CauseID,
		Actor:        d.skill.Manifest.ID + "@" + d.skill.Manifest.Version,
		Capability:   d.skill.Manifest.Capability,
		Decision:     decision,
		Outcome:      spec.OutcomeDelivered,
		Policy:       s.policy.Hash(),
		Payload:      out.Payload,
		Compensation: compensationOf(d.skill.Manifest),
		Compensates:  s.undoOf,
		Inference:    inference,
	}
	receipt, err := s.ldg.Seal(req)
	if err != nil {
		s.log.Error("effect sealing failed", "session", s.ID, "capability", req.Capability, "err", err)
		return
	}
	out.Receipt = receipt
}

// sealDenial attests a gated effect a human refused. Unlike a delivered
// effect there is no outgoing envelope to attach a receipt to — the entry
// exists purely as the record that the proposal was made and refused, which
// is exactly the case an auditor asking "what did this session try to do"
// needs the ledger to answer, not only "what did it succeed at".
func (s *Session) sealDenial(held pendingGate) {
	if s.ldg == nil || held.to.skill == nil {
		return
	}
	// A refused effect cites the same inferences a delivered one would. What
	// argued for an act is worth recording whether or not the act happened —
	// "which model kept proposing the payment a human kept refusing" is a
	// question the ledger should be able to answer.
	inference, _ := s.attests.get(s.rootOf(held.env))
	req := ledger.SealRequest{
		Session: s.ID, Envelope: held.env.ID, Cause: held.env.CauseID,
		Actor:        held.to.skill.Manifest.ID + "@" + held.to.skill.Manifest.Version,
		Capability:   held.to.skill.Manifest.Capability,
		Decision:     spec.DecisionGate,
		Outcome:      spec.OutcomeDenied,
		Policy:       s.policy.Hash(),
		Payload:      held.env.Payload,
		Compensation: compensationOf(held.to.skill.Manifest),
		Compensates:  s.undoOf,
		Inference:    inference,
	}
	if _, err := s.ldg.Seal(req); err != nil {
		s.log.Error("denial sealing failed", "session", s.ID, "capability", req.Capability, "err", err)
	}
}

// compensationOf translates a skill's declared C1 `compensates` (if any) into
// the ledger's Compensation shape. Nil means the effect is recorded as
// irreversible, not that compensation was skipped.
func compensationOf(m registry.Manifest) *ledger.Compensation {
	if m.Compensates == nil {
		return nil
	}
	return &ledger.Compensation{
		Capability: m.Capability, Port: m.Compensates.Port, Schema: m.Compensates.Schema,
	}
}

func (s *Session) emitError(causeID, msg string) {
	payload, _ := json.Marshal(map[string]string{"state": "error", "detail": msg})
	env := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: causeID,
		Session: s.ID, Node: ClientRef, Port: "text_in",
		Kind: channel.KindError, Schema: "std/status@1", Payload: payload,
	}
	raw, _ := json.Marshal(env)
	_ = s.st.AppendEvent(s.ID, env.ID, env.CauseID, env.Kind, raw)
	if err := s.sendClient(raw, channel.QoSReliable); err != nil {
		s.log.Error("error delivery failed", "err", err)
	}
}
