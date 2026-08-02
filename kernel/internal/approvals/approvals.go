// Package approvals lets a human answer a kernel gate that nobody is watching.
//
// The human-approval gate (C2 rule 4) is answered on the client socket: the
// executor sends `confirm_request` to whoever opened the session, and that
// client replies. For a person at the control-plane UI that works, because the
// person and the client are the same entity.
//
// It stops working the moment the client is a program. The MCP server is the
// case that forced this package: an agent's tool call arrives over `POST /mcp`,
// the executor gates it because the capability is `motor.*`, and the
// confirm_request goes to a projection with no human attached. Until now that
// resolved the only honest way it could — an automatic deny with an explanation
// telling the operator to re-run the thing from the UI. Safe, and useless: it
// meant `aura guard` could front read-only tools and nothing else, which is the
// opposite of the interesting half.
//
// So the question needs somewhere to wait. A pending approval is parked here,
// the caller blocks, and any surface the operator already has — the UI, the
// HTTP API, `aura approve` — answers it by id. The gate itself does not move:
// the executor still decides that approval is required and still refuses to
// deliver without it. What changes is only that the question can now reach a
// person who is not holding the socket.
//
// Deliberately in memory and deliberately not durable. A pending approval is a
// question someone is waiting on right now; a node that restarts has dropped
// the call that asked it, and reviving the question afterwards would invite
// approving an effect whose requester is long gone. The *decision* is durable —
// the executor seals it into the ledger either way — which is the part that has
// to survive.
package approvals

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"aura/kernel/internal/channel"
)

// DefaultTTL bounds how long a question waits before it answers itself with a
// denial. An approval that never resolves would hold an MCP call open until the
// agent's own timeout, and a caller that cannot tell "denied" from "hung" will
// retry — which is how one gated effect becomes several.
const DefaultTTL = 5 * time.Minute

// Pending is one unanswered question, as an operator sees it.
type Pending struct {
	ID string `json:"id"`
	// Question is the executor's own wording, passed through unchanged.
	Question string `json:"question"`
	// Origin names what asked — "mcp", "ui", a projection name. An operator
	// approving a write deserves to know which surface requested it.
	Origin     string          `json:"origin,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	Capability string          `json:"capability,omitempty"`
	Session    string          `json:"session,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Requested  time.Time       `json:"requested"`
	Expires    time.Time       `json:"expires"`
}

type waiter struct {
	p  Pending
	ch chan bool
}

// Registry holds the questions currently waiting.
type Registry struct {
	mu    sync.Mutex
	items map[string]*waiter
	ttl   time.Duration
}

func New() *Registry { return &Registry{items: map[string]*waiter{}, ttl: DefaultTTL} }

// WithTTL returns a registry that expires questions after d. Used by tests to
// avoid waiting out the real timeout.
func WithTTL(d time.Duration) *Registry {
	return &Registry{items: map[string]*waiter{}, ttl: d}
}

// Open parks a question and returns the channel its answer arrives on.
//
// The returned release function must be called by the caller when it stops
// waiting — otherwise a caller that gave up leaves the question listed, and an
// operator approves an effect that will never be delivered.
func (r *Registry) Open(p Pending) (id string, decision <-chan bool, release func()) {
	if p.ID == "" {
		p.ID = channel.NewID()
	}
	p.Requested = time.Now()
	p.Expires = p.Requested.Add(r.ttl)

	w := &waiter{p: p, ch: make(chan bool, 1)}
	r.mu.Lock()
	r.items[p.ID] = w
	r.mu.Unlock()

	// The expiry timer denies rather than simply forgetting: a caller blocked
	// on the channel has to be released, and "nobody answered in time" is a
	// refusal, not an approval.
	t := time.AfterFunc(r.ttl, func() { _ = r.Resolve(p.ID, false) })

	return p.ID, w.ch, func() {
		t.Stop()
		r.mu.Lock()
		delete(r.items, p.ID)
		r.mu.Unlock()
	}
}

// Resolve answers a question. It is safe to call twice; the second call reports
// that the question is gone rather than double-delivering.
func (r *Registry) Resolve(id string, approve bool) error {
	r.mu.Lock()
	w, ok := r.items[id]
	if ok {
		delete(r.items, id)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending approval %q (already answered, expired, or the caller gave up)", id)
	}
	w.ch <- approve
	close(w.ch)
	return nil
}

// List returns the questions still waiting, oldest first.
func (r *Registry) List() []Pending {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Pending, 0, len(r.items))
	for _, w := range r.items {
		out = append(out, w.p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Requested.Before(out[j-1].Requested); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Get returns one pending question.
func (r *Registry) Get(id string) (Pending, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.items[id]
	if !ok {
		return Pending{}, false
	}
	return w.p, true
}
