package executor

// Speculative graph execution — starting downstream work on a partial upstream
// output, and throwing it away if the output turns out differently.
//
// # Why this is safe here and delicate everywhere else
//
// The dominant cost in an agent graph is sequential waiting: a model streams
// for two seconds, and only when it stops does the next skill begin. Running
// the next skill early on the prefix is an obvious win and a well-studied one
// (PASTE, SPORK, SpecBox, Dynamic Speculative Agent Planning — a whole 2026
// literature). Every one of those systems spends most of its effort on the
// same question: **which steps are safe to run before we are sure?** Running a
// tool early that sends an email, charges a card or writes a row is not a
// latency optimisation, it is a bug with a stopwatch. They answer it with
// heuristics, allow-lists, or an LLM's judgement.
//
// This runtime does not have to ask. C1's five skill types are an *effect
// type system*: `motor` is precisely "acts on the world", and the kernel
// already treats it specially for the approval gate (C2 rule 5) and for the
// effect ledger (C4). So the answer is already in the manifest, statically,
// for every skill in the graph:
//
//	an edge into a motor skill can never be speculative — refused at wiring
//	everything else is a pure computation whose discarded result costs only time
//
// That refusal lives in applyPolicy, next to the motor gate, because it is the
// same invariant seen from a different angle: the type that needs a human
// before it runs is the type that must not run early.
//
// # What a speculation actually is
//
// A streaming producer emits partials. On a speculative edge the kernel folds
// those partials into a running value and delivers it downstream *as if it
// were final*, marked speculative. When the true final arrives:
//
//	the accumulated value is unchanged  → the speculation was right; the
//	                                      redundant final delivery is suppressed
//	                                      and the downstream work already done
//	                                      stands. This is the win.
//	the accumulated value differs       → the speculative chain is cancelled
//	                                      through the ordinary C3 cancel path,
//	                                      and the real value is delivered.
//
// A miss costs the downstream work that was thrown away and nothing else: the
// cancel machinery already guarantees that anything a cancelled chain emits
// goes nowhere, so a skill that ignores the cancel cannot leak a speculative
// result into a real one.
//
// # Folding partials
//
// Accumulation is schema-directed, reusing a distinction C1 already draws and
// the README already explains at length: in `std/text@1` the `text` field is a
// *delta* to append, while in `std/transcript@1` it *replaces* the previous
// hypothesis, because a recogniser re-decodes its whole buffer. Getting this
// backwards would make every speculation a miss for one of the two, which is
// why the rule is read off the schema rather than guessed.

import (
	"encoding/json"
	"sync"
)

const (
	maxSpecRoots = 1024 // chains tracked for speculation at once
)

// SpeculationStats is what a node reports about its own guessing. Exposed on
// /healthz so an operator can tell a feature that is paying for itself from
// one that is burning compute on misses.
type SpeculationStats struct {
	// Attempts is how many speculative deliveries were made.
	Attempts int64 `json:"attempts"`
	// Hits are speculations the final output confirmed — a suppressed
	// redundant delivery and downstream work that was already done.
	Hits int64 `json:"hits"`
	// Misses are speculations the final output contradicted, whose downstream
	// work was cancelled and repeated.
	Misses int64 `json:"misses"`
	// Refused counts edges that asked to speculate and were denied, either by
	// the motor invariant or by node policy.
	Refused int64 `json:"refused"`
}

// specKey identifies one speculative stream: a causal chain and the edge
// endpoint feeding it.
type specKey struct {
	root string
	from string // "ref.port" of the producer
}

// specState is what has been accumulated and what has been guessed for one
// stream.
type specState struct {
	// accumulated is the folded payload as of the last partial seen.
	accumulated json.RawMessage
	// speculatedOn is the accumulated value the most recent speculative
	// delivery was based on, or nil if nothing has been speculated yet.
	speculatedOn json.RawMessage
	// deliveries are the envelope ids handed downstream speculatively, so
	// they can be cancelled on a miss.
	deliveries []string
}

// specIndex tracks speculation per chain. Bounded like every other per-session
// index — a long-lived voice session must not grow the node's memory.
type specIndex struct {
	mu    sync.Mutex
	state map[specKey]*specState
	order []specKey
	max   int

	stats SpeculationStats
}

func newSpecIndex(max int) *specIndex {
	return &specIndex{state: map[specKey]*specState{}, max: max}
}

func (x *specIndex) get(k specKey) *specState {
	st, ok := x.state[k]
	if ok {
		return st
	}
	st = &specState{}
	x.state[k] = st
	x.order = append(x.order, k)
	if len(x.order) > x.max {
		oldest := x.order[0]
		x.order = x.order[1:]
		delete(x.state, oldest)
	}
	return st
}

// fold accumulates one partial payload and returns the running value.
func (x *specIndex) fold(k specKey, schema string, payload json.RawMessage) json.RawMessage {
	x.mu.Lock()
	defer x.mu.Unlock()
	st := x.get(k)
	st.accumulated = accumulate(schema, st.accumulated, payload)
	return st.accumulated
}

// speculate records that a speculative delivery is being made on the current
// accumulated value, and returns the envelope ids to cancel if it misses.
func (x *specIndex) speculate(k specKey, envelopeID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st := x.get(k)
	st.speculatedOn = st.accumulated
	st.deliveries = append(st.deliveries, envelopeID)
	x.stats.Attempts++
}

// resolve compares the final accumulated value against what was speculated on
// and forgets the stream. `hit` is true when the speculation stands; `stale`
// lists the speculative envelope ids that must be cancelled on a miss.
func (x *specIndex) resolve(k specKey, final json.RawMessage) (hit bool, stale []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, tracked := x.state[k]
	if !tracked || st.speculatedOn == nil {
		return false, nil
	}
	hit = contentEqual(st.speculatedOn, final)
	if hit {
		x.stats.Hits++
	} else {
		x.stats.Misses++
		stale = append(stale, st.deliveries...)
	}
	x.forget(k)
	return hit, stale
}

// forget removes a stream. Caller must hold mu.
func (x *specIndex) forget(k specKey) {
	delete(x.state, k)
	for i, existing := range x.order {
		if existing == k {
			x.order = append(x.order[:i], x.order[i+1:]...)
			break
		}
	}
}

// Stats returns a snapshot.
func (x *specIndex) Stats() SpeculationStats {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.stats
}

// accumulate folds a partial into a running value according to the schema's
// own semantics.
//
// The two cases are not interchangeable, and C1 says so: `std/text@1` carries
// a delta, `std/transcript@1` carries a replacement. Anything else is treated
// as a replacement, which is the safe default — a wrong replacement produces a
// speculation miss, while a wrong concatenation would produce a *hit* on
// garbage.
func accumulate(schema string, running, next json.RawMessage) json.RawMessage {
	if schema != schemaText {
		return next
	}
	var acc, incoming textPayload
	if len(running) > 0 {
		_ = json.Unmarshal(running, &acc)
	}
	if err := json.Unmarshal(next, &incoming); err != nil {
		return next
	}
	acc.Text += incoming.Text
	acc.Final = incoming.Final
	out, err := json.Marshal(acc)
	if err != nil {
		return next
	}
	return out
}

// schemaText is the one schema whose payloads concatenate.
const schemaText = "std/text@1"

type textPayload struct {
	Text  string `json:"text"`
	Final bool   `json:"final,omitempty"`
}

// isFinal reports whether a payload marks the end of its logical stream.
//
// Read off the payload rather than the envelope kind because a streaming skill
// signals completion with `final: true` on a `data` envelope and only then
// sends `done`; waiting for `done` would resolve every speculation one hop too
// late.
func isFinal(payload json.RawMessage) bool {
	if len(payload) == 0 {
		return false
	}
	var probe struct {
		Final *bool `json:"final"`
	}
	if json.Unmarshal(payload, &probe) != nil || probe.Final == nil {
		return false
	}
	return *probe.Final
}

// markFinal sets `final: true` on a payload, so a speculative delivery reads
// to the consumer as a complete input rather than as one more partial.
//
// A no-op for a payload with no object shape: a schema that does not model
// completion has nothing to mark, and inventing a field would violate the very
// port type this runtime enforces.
func markFinal(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return raw
	}
	obj["final"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// contentEqual reports whether two payloads carry the same *content*,
// disregarding key order, whitespace, and the `final` flag.
//
// Ignoring `final` is not a shortcut, it is the definition. A speculation asks
// "will the value turn out to be what I guessed?"; `final` answers a different
// question — "has the stream ended?" — and by construction it is false on
// every partial and true on the last one. Comparing it would make *every*
// speculation a miss, no matter how right the guess was, which is exactly what
// the first build of this did.
func contentEqual(a, b json.RawMessage) bool {
	return canonicalContent(a) == canonicalContent(b)
}

// canonicalContent renders a payload with `final` removed and keys sorted, so
// two encoders producing the same value compare equal.
func canonicalContent(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	if obj, ok := v.(map[string]any); ok {
		delete(obj, "final")
	}
	out, err := json.Marshal(v) // encoding/json sorts map keys
	if err != nil {
		return string(raw)
	}
	return string(out)
}
