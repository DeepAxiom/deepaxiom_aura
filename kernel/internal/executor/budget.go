package executor

// Deadlines and the context budget — two limits the kernel enforces because a
// limit enforced by the skill is a limit that holds only for skills that
// remembered to implement it.
//
// This is the same argument that put the motor gate in the executor. A context
// window blown in `llm-chat` is a truncation nobody notices; blown in a
// projected API it is a 413; blown in a memory skill it is silent data loss.
// One place, one rule, one error message.

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/spec"
)

// bounds are the two instants a delivery travels under. Zero means absent.
//
// They are independent and a delivery may be under both, which is why they
// are two fields and not one instant with a meaning attached:
//
//   - validUntil (C3 `deadline`) is a *validity* bound. Past it, whatever this
//     delivery causes is worthless — an act on a scene that has since changed
//     — and the kernel drops it.
//   - respondBy (C3 `respond_by`) is a *responsiveness* target. Past it, the
//     reply is late and still wanted; the kernel records the miss and delivers.
type bounds struct {
	validUntil int64
	respondBy  int64
}

// inherited is what an emission carries forward from the delivery that caused
// it: the bounds, and what the kernel could establish about the link.
type inherited struct {
	bounds
	// prev is the delivery that caused the emission. Empty at the head of a
	// chain and whenever the stated cause could not be verified.
	prev string
	link linkState
}

// tighten returns the earlier of two instants, treating zero as "none".
//
// Earlier always wins: a chain given 200ms cannot be extended by a downstream
// edge asking for 500ms, or the budget would be advisory. A *tighter* local
// bound is honoured, because asking for less time than you were given is
// always safe.
func tighten(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

// boundFor computes the bounds of a delivery to d: what its source inherited,
// tightened by what the edge itself declares.
//
// The edge's own budget starts at the moment the hop is attempted, which for
// a gated delivery is when it is held, not when a human releases it — see
// holdForApproval, which calls this once and parks the result.
func boundFor(inh bounds, d dest, now time.Time) bounds {
	b := inh
	if d.deadlineMS > 0 {
		b.validUntil = tighten(b.validUntil,
			now.Add(time.Duration(d.deadlineMS)*time.Millisecond).UnixMilli())
	}
	if d.respondMS > 0 {
		b.respondBy = tighten(b.respondBy,
			now.Add(time.Duration(d.respondMS)*time.Millisecond).UnixMilli())
	}
	return b
}

// past reports whether an absolute instant has gone by.
func past(instant int64, now time.Time) bool {
	return instant > 0 && now.UnixMilli() > instant
}

// deadlineCounters are node-wide: what the two bounds did here.
type deadlineCounters struct {
	dropped        atomic.Uint64
	effectsExpired atomic.Uint64
	lateResponses  atomic.Uint64
}

// DeadlineStats is what a node reports about the two bounds.
type DeadlineStats struct {
	// Dropped counts `data` deliveries not made because their `deadline` had
	// passed.
	Dropped uint64 `json:"dropped"`
	// EffectsExpired is the part of Dropped that were effects: deliveries into
	// a motor Expertise, each sealed in the ledger as `expired`.
	EffectsExpired uint64 `json:"effects_expired"`
	// LateResponses counts first outputs delivered after their `respond_by`.
	// Nothing was dropped; this is how late the node is at answering.
	LateResponses uint64 `json:"late_responses"`
}

func (c *deadlineCounters) snapshot() DeadlineStats {
	return DeadlineStats{
		Dropped:        c.dropped.Load(),
		EffectsExpired: c.effectsExpired.Load(),
		LateResponses:  c.lateResponses.Load(),
	}
}

// inherit works out what an emission carries forward.
//
// This is where C2 rule 7 — "deadlines are absolute and inherited" — is
// actually implemented. It used to be one line in forward that read the
// deadline off the envelope the Expertise emitted, and no SDK ever put one
// there, so the inheritance the contract promised stopped at the first
// Expertise in any chain. It also meant the Expertise was the one carrying its
// own budget, and so could have extended it.
//
// Now the Expertise carries nothing. It names the delivery it is answering
// (`cause_id`, as it always has), the kernel looks that delivery up, and the
// bounds come from the kernel's own record of it.
func (s *Session) inherit(env channel.Envelope, em Emitter) inherited {
	if em.trusted() {
		// The kernel's own authority: a client envelope, an ingress route, a
		// pinned output. A caller that starts a chain may put a bound on it,
		// and that is taken as given.
		return inherited{
			link:   linkRoot,
			bounds: bounds{validUntil: env.Deadline, respondBy: env.RespondBy},
		}
	}
	if env.CauseID == "" {
		return inherited{link: linkRoot}
	}
	cause, ok := s.lineage.touch(env.CauseID)
	if !ok {
		// Evicted, or never a delivery. Either way there is nothing to inherit
		// from, and saying so is better than guessing: an emission that kept
		// some other delivery's deadline would be dropped for a budget it was
		// never under.
		s.lineage.stats.unverified.Add(1)
		return inherited{link: linkUnverified}
	}
	if !cause.madeTo(em) {
		s.lineage.stats.foreign.Add(1)
		s.log.Warn("an Expertise emitted under a delivery that was made to another",
			"session", s.ID, "emitter", em.Expertise, "conn", em.Conn,
			"cause", env.CauseID, "delivered_to", cause.expertise)
		return inherited{link: linkForeign}
	}
	inh := inherited{link: linkVerified, prev: cause.id}
	inh.validUntil = cause.validUntil
	// respond_by bounds the *front* of what a delivery causes. The first
	// `data` output takes it and carries it on; everything behind the front is
	// under no responsiveness target at all, which is the point — a reply that
	// started in time is not late for being long.
	if cause.respondBy != 0 && env.Kind == channel.KindData && s.lineage.takeFront(cause.id) {
		inh.respondBy = cause.respondBy
	}
	return inh
}

// contextLedger tracks how much context a session has accumulated against its
// graph's budget.
//
// "Tokens" here are estimated, not tokenized. The kernel has no tokenizer and
// should not grow one — that would couple it to a model family, which is the
// coupling the whole architecture avoids. The estimate is bytes/4, the
// long-standing rule of thumb for English-ish text under BPE, and it is
// deliberately *conservative in the direction that matters*: it under-counts
// nothing important, and a budget set from measured usage will hold.
//
// Being explicit that this is an estimate is the point. A budget presented as
// exact would be trusted for capacity planning it cannot support.
type contextLedger struct {
	mu       sync.Mutex
	budget   int
	consumed int
}

func newContextLedger(budget int) *contextLedger {
	return &contextLedger{budget: budget}
}

// bytesPerTokenEstimate is the divisor turning payload bytes into an
// approximate token count.
const bytesPerTokenEstimate = 4

// charge adds a payload's estimated cost and reports whether the budget is
// blown.
func (c *contextLedger) charge(payloadBytes int) (consumed int, over bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.budget <= 0 {
		return c.consumed, false
	}
	c.consumed += payloadBytes / bytesPerTokenEstimate
	return c.consumed, c.consumed > c.budget
}

// chargeContext accounts one delivery against the graph's context budget.
//
// Only `data` envelopes count: a status or a done carries no context into a
// model. Only deliveries into a skill count — what a session sends back to the
// client is not context anybody has to fit in a window.
func (s *Session) chargeContext(out channel.Envelope) error {
	if s.ctxLedger == nil || out.Kind != channel.KindData || out.Node == ClientRef {
		return nil
	}
	consumed, over := s.ctxLedger.charge(len(out.Payload))
	if !over {
		return nil
	}
	return fmt.Errorf(
		"context budget exhausted: this graph declared context_budget %d and the session "+
			"has accumulated roughly %d tokens. Raise the budget, or wire a "+
			"logical.context.compress skill ahead of this edge",
		s.ctxLedger.budget, consumed)
}

// dropExpired reports whether a delivery has missed its deadline, and records
// why in the event log so a disappearing message is explicable.
//
// Deliberately checked at the moment of delivery rather than by a timer: the
// question "is this still worth sending" only has an answer when there is
// something to send, and a timer would need one goroutine per in-flight
// envelope to answer it earlier.
//
// And decided *before* the Effect Checkpoint. The order used to be the
// reverse — seal, then check — which was harmless only because the check
// could not fire: the deadline was computed at that same hop, a moment
// earlier. With inheritance working it can, and the old order would have
// sealed an effect as `delivered`, handed out its receipt, and then not
// delivered it. A ledger that says an act happened when it did not is worse
// than no ledger.
func (s *Session) dropExpired(out channel.Envelope, d dest, root, causeID string, now time.Time) bool {
	if !past(out.Deadline, now) {
		return false
	}
	// A terminal envelope always travels: dropping a `done` or an `error`
	// leaves the consumer waiting forever for something that already happened,
	// which is a worse outcome than a late one.
	if out.Kind != channel.KindData {
		return false
	}
	late := now.UnixMilli() - out.Deadline
	s.log.Debug("dropping a delivery past its deadline",
		"session", s.ID, "to", d.ref+"."+d.port, "late_ms", late)
	s.deadlines.dropped.Add(1)

	// An effect that did not happen is still something this session tried to
	// do, and "why did the arm not stop" is a question the ledger should be
	// able to answer on its own. Sealed against the real payload, like any
	// other entry, before the payload is elided from the log below.
	if d.skill != nil && d.skill.Manifest.Type == TypeMotor {
		s.deadlines.effectsExpired.Add(1)
		if _, err := s.seal(out, d, root, spec.OutcomeExpired); err != nil {
			s.log.Error("sealing an expired effect failed", "session", s.ID,
				"capability", d.skill.Manifest.Capability, "err", err)
		}
	}

	payload, _ := json.Marshal(map[string]any{
		"elided":   true,
		"deadline": out.Deadline,
		"late_ms":  late,
		"reason":   "past deadline",
	})
	dropped := out
	dropped.Payload = payload
	raw, _ := json.Marshal(dropped)
	_ = s.st.AppendEvent(s.ID, dropped.ID, dropped.CauseID, dropped.Kind, raw)

	// Told once per chain and destination, not once per dropped token: a
	// reply cut short should be explicable to whoever is watching it, and a
	// status per frame would be a second stream of noise beside the first.
	if note := root + "|" + d.ref + "." + d.port; !s.expiryNoted.has(note) {
		s.expiryNoted.add(note)
		status, _ := json.Marshal(map[string]any{
			"state":    "working",
			"event":    "deadline_exceeded",
			"node":     d.ref,
			"port":     d.port,
			"deadline": out.Deadline,
			"late_ms":  late,
			"detail": fmt.Sprintf("deliveries to %s.%s are being dropped: this chain's deadline "+
				"passed %dms ago", d.ref, d.port, late),
		})
		s.emitStatus(causeID, status)
	}
	return true
}

// noteLateResponse records a first output that arrived after its target.
//
// Nothing is dropped — that is the difference between this and a deadline —
// so all that happens is that it becomes visible: a counter, and one event in
// the causal log beside the delivery that was late.
func (s *Session) noteLateResponse(out channel.Envelope, d dest, now time.Time) {
	late := now.UnixMilli() - out.RespondBy
	s.deadlines.lateResponses.Add(1)
	s.log.Debug("first output delivered after its respond_by",
		"session", s.ID, "to", d.ref+"."+d.port, "late_ms", late)
	payload, _ := json.Marshal(map[string]any{
		"state":      "working",
		"event":      "response_late",
		"node":       d.ref,
		"port":       d.port,
		"respond_by": out.RespondBy,
		"late_ms":    late,
	})
	// No node or port: it is the kernel's note about a delivery, not a
	// delivery, and resume counts anything addressed to a destination toward
	// that hop's sequence.
	note := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: out.CauseID,
		Session: s.ID, Kind: channel.KindStatus, Schema: "std/status@1", Payload: payload,
	}
	raw, _ := json.Marshal(note)
	_ = s.st.AppendEvent(s.ID, note.ID, note.CauseID, note.Kind, raw)
}
