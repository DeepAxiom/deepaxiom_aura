package executor

import (
	"encoding/json"

	"aura/kernel/internal/channel"
)

// Session resume (Phase 2). Not a mode: a postcondition. NewSession calls
// resumeFromLog once, after routes is built, on every session it constructs
// — a brand-new session id has no log yet, so this is a no-op, and a
// reconnect (client dropped and came back, or the kernel process itself
// restarted) gets exactly the same treatment as a first-time start. Nothing
// here re-delivers to a skill, re-seals an effect (C4), or appends to the
// log again — it only rebuilds the ephemeral bookkeeping (dedup window,
// causal/in-flight indexes, delivery lineage, pending gates, per-hop Seq
// counters) that was never durable in the first place, from the causal event
// log C3 rule 7 already guarantees exists.
//
// The whole log, in bounded memory. It used to read the first thousand events
// and stop, so a session resumed past its thousandth event came back with the
// dedup window, the hop counters and the pending gates of its first minute —
// and what matters after a restart is the *end* of the history. It also held
// every envelope it read in a map, which is fine for a thousand and is the
// whole session otherwise. Now it walks the log a page at a time and keeps
// only what something later in the walk can still refer to.

// resumeHeld bounds how many emissions resume remembers as possibly held at a
// gate. A confirm_request is written immediately after the envelope it holds,
// so the envelope is always among the most recent few; the bound is generous
// only because inline dispatch lets other connections interleave.
const resumeHeld = 1024

// recent is a bounded, oldest-first-out map: what a single pass over history
// needs to remember about the part it has already walked.
type recent[V any] struct {
	byID  map[string]V
	order []string
	max   int
}

func newRecent[V any](max int) *recent[V] {
	return &recent[V]{byID: make(map[string]V), max: max}
}

func (r *recent[V]) put(id string, v V) {
	if id == "" {
		return
	}
	if _, exists := r.byID[id]; !exists {
		r.order = append(r.order, id)
		if len(r.order) > r.max {
			delete(r.byID, r.order[0])
			r.order = r.order[1:]
		}
	}
	r.byID[id] = v
}

func (r *recent[V]) get(id string) (V, bool) {
	v, ok := r.byID[id]
	return v, ok
}

// resumeFromLog seeds s's bookkeeping from its own persisted history.
func (s *Session) resumeFromLog() error {
	// ref.port -> dest, built once. Every destination's own ref/port identify
	// it uniquely regardless of which source edge feeds it — routes itself is
	// keyed by *source*, so this is the index reconstruction needs to find
	// "what was this envelope delivered to" from Node/Port alone.
	byDest := map[string]dest{}
	// Sources that feed a gated edge: the only envelopes that can be sitting
	// held, and so the only ones worth remembering whole.
	gatedSource := map[string]bool{}
	for from, dests := range s.routes {
		for _, d := range dests {
			byDest[d.ref+"."+d.port] = d
			if d.gate == GateHumanApproval {
				gatedSource[from] = true
			}
		}
	}

	type candidate struct {
		req  channel.Envelope
		held channel.Envelope
	}
	pending := map[string]candidate{} // confirm_request id -> the request and what it holds
	holdable := newRecent[channel.Envelope](resumeHeld)
	// emission id -> its stated cause. A delivery's cause is the emission it
	// forwarded; that emission's cause is the delivery before it. This is the
	// one hop of memory that turns the log back into lineage.
	causeOf := newRecent[string](2 * maxLineage)
	maxSeq := map[string]uint64{} // "ref.port" -> highest Seq observed

	err := s.st.ScanSession(s.ID, func(raw []byte, _ int64) bool {
		var env channel.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return true // a corrupt row must not block resume for everything after it
		}

		// rootOf is safe mid-replay: a parent envelope always precedes its
		// children in log order, so by the time an envelope's CauseID is
		// looked up here, causal already holds whatever entry it needs. This
		// also harmlessly records a self-referential root for a genuinely
		// client-originated message (rootOf returns its own id) and for
		// non-data kinds (confirm_request, cancel, …) — those entries are
		// simply never read back, the same way forward()'s own causal.set
		// only ever matters for entries something else's CauseID points at.
		root := s.rootOf(env)
		if root != "" {
			s.causal.set(env.ID, root)
		}

		// An envelope whose Node/Port match a known destination is one a
		// historical forward() delivered — the same test forward() itself
		// effectively performs by construction (out.Node, out.Port = d.ref,
		// d.port). Every such delivery, regardless of kind, consumed a slot
		// in that hop's Seq counter live, so the max has to account for all
		// of them; only `data` ones represent ongoing work worth telling a
		// future cancel about.
		key := env.Node + "." + env.Port
		if d, ok := byDest[key]; ok {
			if env.Seq > maxSeq[key] {
				maxSeq[key] = env.Seq
			}
			if env.Kind == channel.KindData && root != "" {
				s.inFlight.record(root, hop{to: d, causeID: env.CauseID})
			}
			if d.skill != nil {
				// A delivery to an Expertise: back into the lineage, with the
				// bounds it was actually sent under (they are on the wire, so
				// they are in the log) and the delivery before it, when that
				// one is still within the index.
				prev := ""
				if before, ok := causeOf.get(env.CauseID); ok {
					if _, known := s.lineage.peek(before); known {
						prev = before
					}
				}
				s.lineage.record(delivery{
					id: env.ID, prev: prev, root: root,
					cause: env.CauseID, priority: d.priority,
					ref: d.ref, port: d.port,
					expertise: d.skill.Manifest.ID, token: d.skill.TokenID,
					validUntil: env.Deadline, respondBy: env.RespondBy,
				})
			}
		} else {
			// Not a delivery: something a source put on the session.
			causeOf.put(env.ID, env.CauseID)
			if env.Kind == channel.KindData && env.Node != ClientRef {
				// The front of that delivery's output had already gone out.
				s.lineage.markFront(env.CauseID)
			}
		}
		// Outside the test above on purpose: a port name can be both an
		// egress somewhere and an ingress somewhere else, and an envelope held
		// at a gate is found by where it came *from*.
		if gatedSource[key] {
			holdable.put(env.ID, env)
		}

		switch env.Kind {
		case channel.KindCancel:
			// Mirrors handleCancel's own root resolution exactly: a cancel's
			// root comes from what it names (CauseID), never from itself. No
			// cancel envelopes are re-sent here — inFlight.take() is a live
			// consume operation, and whatever was in flight at the moment of
			// this historical cancel was already asked to stop; what matters
			// after resume is that the root stays suppressed, which s.cancelled
			// alone guarantees (Route checks it before routing anything new).
			croot := env.CauseID
			if r, ok := s.causal.get(env.CauseID); ok {
				croot = r
			}
			s.cancelled.add(croot)
		case channel.KindConfirmRequest:
			// Captured now, while the envelope it holds is still among the
			// recent ones (holdForApproval sets a confirm_request's CauseID
			// to the held envelope's own id).
			if held, ok := holdable.get(env.CauseID); ok {
				pending[env.ID] = candidate{req: env, held: held}
			}
		case channel.KindConfirmResponse:
			delete(pending, env.CauseID) // resolved — no longer pending
		case channel.KindData:
			if env.Idem != "" {
				s.dedup.Seen(env.Idem)
			}
		}
		return true
	})
	if err != nil {
		return err
	}

	for key, max := range maxSeq {
		s.seq[key] = max
	}

	// Whatever confirm_request survived the pass with no matching response is
	// still pending. Its dest is re-derived from the routes table, scoped to
	// the held envelope's *source* — not the byDest index above, which cannot
	// distinguish two simultaneous human-approval gates reachable from the
	// same source port. to_ref/to_port on the payload (additive,
	// std/confirmation@1) is what makes that match unambiguous.
	for reqID, c := range pending {
		var body struct {
			ToRef  string `json:"to_ref"`
			ToPort string `json:"to_port"`
			// The budget the delivery was held under, as the question stated
			// it. Read back rather than recomputed: recomputing from the edge
			// now would grant a delivery held before a restart a second,
			// fresh budget for having survived it.
			ExpiresAt int64 `json:"expires_at"`
			RespondBy int64 `json:"respond_by"`
		}
		_ = json.Unmarshal(c.req.Payload, &body)
		for _, d := range s.routes[c.held.Node+"."+c.held.Port] {
			if d.ref == body.ToRef && d.port == body.ToPort && d.gate == GateHumanApproval {
				inh := inherited{
					link:   linkRoot,
					bounds: bounds{validUntil: body.ExpiresAt, respondBy: body.RespondBy},
				}
				if c.held.Node != ClientRef {
					if _, known := s.lineage.peek(c.held.CauseID); known {
						inh.prev, inh.link = c.held.CauseID, linkVerified
					} else {
						inh.link = linkUnverified
					}
				}
				s.pendMu.Lock()
				s.pending[reqID] = pendingGate{env: c.held, to: d, inh: inh}
				s.pendMu.Unlock()
				break
			}
		}
	}
	return nil
}
