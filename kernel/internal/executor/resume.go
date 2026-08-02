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
// causal/in-flight indexes, pending gates, per-hop Seq counters) that was
// never durable in the first place, from the causal event log C3 rule 7
// already guarantees exists.

// resumeFromLog seeds s's bookkeeping from its own persisted history.
func (s *Session) resumeFromLog() error {
	raw, _, err := s.st.SessionEvents(s.ID, 0)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}

	// ref.port -> dest, built once. Every destination's own ref/port identify
	// it uniquely regardless of which source edge feeds it — routes itself is
	// keyed by *source*, so this is the index reconstruction needs to find
	// "what was this envelope delivered to" from Node/Port alone.
	byDest := map[string]dest{}
	for _, dests := range s.routes {
		for _, d := range dests {
			byDest[d.ref+"."+d.port] = d
		}
	}

	byID := map[string]channel.Envelope{}
	pendingCandidates := map[string]channel.Envelope{} // confirm_request id -> its own envelope
	maxSeq := map[string]uint64{}                      // "ref.port" -> highest Seq observed

	for _, r := range raw {
		var env channel.Envelope
		if err := json.Unmarshal(r, &env); err != nil {
			continue // a corrupt row must not block resume for everything after it
		}
		byID[env.ID] = env

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
			pendingCandidates[env.ID] = env
		case channel.KindConfirmResponse:
			delete(pendingCandidates, env.CauseID) // resolved — no longer pending
		case channel.KindData:
			if env.Idem != "" {
				s.dedup.Seen(env.Idem)
			}
		}
	}

	for key, max := range maxSeq {
		s.seq[key] = max
	}

	// Whatever confirm_request survived the pass with no matching response is
	// still pending. Its held envelope is found by CauseID (holdForApproval
	// sets a confirm_request's CauseID to the held envelope's own id); its
	// dest is re-derived from the routes table, scoped to the held envelope's
	// *source* — not the byDest index above, which cannot distinguish two
	// simultaneous human-approval gates reachable from the same source port.
	// to_ref/to_port on the payload (additive, std/confirmation@1) is what
	// makes that match unambiguous.
	for reqID, req := range pendingCandidates {
		held, ok := byID[req.CauseID]
		if !ok {
			continue
		}
		var body struct {
			ToRef  string `json:"to_ref"`
			ToPort string `json:"to_port"`
		}
		_ = json.Unmarshal(req.Payload, &body)
		for _, d := range s.routes[held.Node+"."+held.Port] {
			if d.ref == body.ToRef && d.port == body.ToPort && d.gate == GateHumanApproval {
				s.pendMu.Lock()
				s.pending[reqID] = pendingGate{env: held, to: d}
				s.pendMu.Unlock()
				break
			}
		}
	}
	return nil
}
