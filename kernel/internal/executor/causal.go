package executor

import "sync"

// Bookkeeping that makes `cancel` reach a whole causal chain (C3).
//
// The problem this solves is not just reach, it is *address*. A cancel names
// the client message to abort, but each skill along the chain knows that work
// by a different `cause_id` — the id of whatever the kernel delivered to it.
// A cancel broadcast with the client's id would be silently ignored by every
// skill past the first hop, because none of them ever saw that id.
//
// So the session keeps three bounded indexes:
//
//	causalIndex   every delivered envelope -> the client message that
//	              originated its chain ("the root")
//	inFlightIndex root -> the latest thing delivered to each destination,
//	              paired with the cause_id that destination will recognise
//	cancelledSet  roots the client has abandoned
//
// All three are bounded and evict oldest-first: a long-lived voice session
// emits envelopes continuously and must not grow the node's memory without
// limit.

const (
	maxCausalRoots  = 8192 // ~800KB/session at 26-char ids
	maxInFlightRoot = 1024
	maxCancelled    = 1024
)

// causalIndex maps an envelope id to the root of its causal chain.
type causalIndex struct {
	mu    sync.Mutex
	root  map[string]string
	order []string
	max   int
}

func newCausalIndex(max int) *causalIndex {
	return &causalIndex{root: make(map[string]string), max: max}
}

func (c *causalIndex) set(envelopeID, root string) {
	if envelopeID == "" || root == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.root[envelopeID]; !exists {
		c.order = append(c.order, envelopeID)
		if len(c.order) > c.max {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.root, oldest)
		}
	}
	c.root[envelopeID] = root
}

func (c *causalIndex) get(envelopeID string) (string, bool) {
	if envelopeID == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.root[envelopeID]
	return r, ok
}

// hop is one destination the kernel delivered to, and the cause_id that
// destination will match a cancel against.
type hop struct {
	to      dest
	causeID string
}

// inFlightIndex remembers, per root, the most recent delivery to each
// destination. Keeping only the latest per destination is deliberate: a
// streaming skill emits an envelope per token, and remembering every one of
// them would grow without bound while adding nothing — the newest delivery is
// the one a skill is most likely still working on, and the kernel's
// suppression (not this index) is what actually guarantees the chain stops.
type inFlightIndex struct {
	mu     sync.Mutex
	byRoot map[string]map[string]hop
	order  []string
	max    int
}

func newInFlightIndex(max int) *inFlightIndex {
	return &inFlightIndex{byRoot: make(map[string]map[string]hop), max: max}
}

func (f *inFlightIndex) record(root string, h hop) {
	if root == "" || h.to.skill == nil {
		return // nothing to ask the client to stop doing
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	dests, exists := f.byRoot[root]
	if !exists {
		dests = map[string]hop{}
		f.byRoot[root] = dests
		f.order = append(f.order, root)
		if len(f.order) > f.max {
			oldest := f.order[0]
			f.order = f.order[1:]
			delete(f.byRoot, oldest)
		}
	}
	dests[h.to.ref+"."+h.to.port] = h
}

// take returns the recorded hops for a root and forgets them: a cancel is
// delivered once.
func (f *inFlightIndex) take(root string) []hop {
	f.mu.Lock()
	defer f.mu.Unlock()
	dests, ok := f.byRoot[root]
	if !ok {
		return nil
	}
	delete(f.byRoot, root)
	for i, r := range f.order {
		if r == root {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	out := make([]hop, 0, len(dests))
	for _, h := range dests {
		out = append(out, h)
	}
	return out
}

// cancelledSet is the bounded set of abandoned roots.
type cancelledSet struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
	max   int
}

func newCancelledSet(max int) *cancelledSet {
	return &cancelledSet{seen: make(map[string]struct{}), max: max}
}

func (c *cancelledSet) add(root string) {
	if root == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.seen[root]; dup {
		return
	}
	c.seen[root] = struct{}{}
	c.order = append(c.order, root)
	if len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.seen, oldest)
	}
}

func (c *cancelledSet) has(root string) bool {
	if root == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.seen[root]
	return ok
}
