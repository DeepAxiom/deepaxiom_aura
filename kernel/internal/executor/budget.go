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
	"time"

	"aura/kernel/internal/channel"
)

// inheritDeadline computes the absolute deadline for a delivery.
//
// An inherited deadline always wins over a looser local one: a chain given
// 200ms total cannot be extended by a downstream edge asking for 500ms, or the
// budget would be advisory. A *tighter* local deadline is honoured, because
// asking for less time than you were given is always safe.
func inheritDeadline(inherited int64, localMS int) int64 {
	var local int64
	if localMS > 0 {
		local = time.Now().Add(time.Duration(localMS) * time.Millisecond).UnixMilli()
	}
	switch {
	case inherited == 0:
		return local
	case local == 0:
		return inherited
	case local < inherited:
		return local
	default:
		return inherited
	}
}

// deadlineExpired reports whether an absolute deadline has passed.
func deadlineExpired(deadline int64) bool {
	return deadline > 0 && time.Now().UnixMilli() > deadline
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

// Consumed reports the running estimate.
func (c *contextLedger) Consumed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consumed
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

// ContextConsumed is the session's running estimate, for /healthz and the UI.
func (s *Session) ContextConsumed() int {
	if s.ctxLedger == nil {
		return 0
	}
	return s.ctxLedger.Consumed()
}

// dropExpired reports whether a delivery has missed its deadline, and records
// why in the event log so a disappearing message is explicable.
//
// Deliberately checked at the moment of delivery rather than by a timer: the
// question "is this still worth sending" only has an answer when there is
// something to send, and a timer would need one goroutine per in-flight
// envelope to answer it earlier.
func (s *Session) dropExpired(out channel.Envelope, d dest) bool {
	if !deadlineExpired(out.Deadline) {
		return false
	}
	// A terminal envelope always travels: dropping a `done` or an `error`
	// leaves the consumer waiting forever for something that already happened,
	// which is a worse outcome than a late one.
	if out.Kind != channel.KindData {
		return false
	}
	late := time.Now().UnixMilli() - out.Deadline
	s.log.Debug("dropping a delivery past its deadline",
		"session", s.ID, "to", d.ref+"."+d.port, "late_ms", late)

	payload, _ := json.Marshal(map[string]any{
		"elided":   true,
		"deadline": out.Deadline,
		"late_ms":  late,
		"reason":   "past deadline",
	})
	dropped := out
	dropped.Payload = payload
	raw, _ := json.Marshal(dropped)
	_ = s.st.AppendEvent(s.ID, dropped.ID, dropped.CauseID, raw)
	return true
}
