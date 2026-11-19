package executor

import (
	"errors"
	"sync/atomic"
)

// What the executor tells something acting on a delivery's behalf.
//
// An Expertise that calls a model is doing so *for* a delivery — the question
// it was handed. Until now nothing outside the Expertise could know which: the
// inference request went straight to an engine and the kernel never saw it.
// Two questions follow from routing it through the node instead, and both are
// answered here, from state the executor already keeps:
//
//   - **Whose work is this?** ResolveDelivery: given the delivery a request
//     names, what chain it belongs to, what priority and what deadline that
//     chain is under, and whether the delivery was made to the party asking.
//   - **Is anyone still waiting for it?** A CancelNotice, raised at the same
//     places the kernel already asks an Expertise to stop.
//
// Deliberately a lookup and a notification, with no notion of what is being
// done on the delivery's behalf. The executor does not know these are
// inference requests, and the package that does (internal/infer) does not
// import this one.

// DeliveryRef is how a caller names the delivery it is working on.
type DeliveryRef struct {
	Session  string
	Delivery string
	// Node and Port are where the delivery arrived. They are what the caller
	// was told, and are used only when the delivery itself is no longer in
	// the lineage index: a priority is a property of the edge, so it can
	// still be read from the graph.
	Node string
	Port string
}

// Party is who is asking. The zero value is nobody.
type Party struct {
	// Operator is the node's own authority: the operator token, or a node
	// with no authentication at all. There is nothing to check such a caller
	// against, and that is stated rather than hidden — see Resolved.Verified.
	Operator bool
	// TokenID and Capability are a scoped credential's: the row it is, and
	// the one capability it may register as.
	TokenID    string
	Capability string
}

// Resolved is what the executor knows about a delivery.
type Resolved struct {
	Session string
	Graph   string
	Root    string
	// Cause is the emission the delivery forwarded — the id an Expertise sees
	// as its `cause_id`, and the one a cancel aimed at this delivery names.
	Cause string
	// Priority is the edge's. Higher wins; zero is ordinary.
	Priority int
	// Deadline and RespondBy are the delivery's two bounds, unix millis, zero
	// when absent.
	Deadline  int64
	RespondBy int64
	// Known is false when the delivery was no longer in the lineage index.
	// Priority then came from the graph, and the bounds are unknown.
	Known bool
	// Verified is true only when the executor could establish that this
	// delivery was made to the party asking: a scoped credential, matching
	// the one the delivery was sent under. With the operator token there is
	// nothing to match, so this is false and the caller is being believed.
	Verified bool
	// Cancelled is true when the chain has already been abandoned.
	Cancelled bool
}

var (
	// ErrUnknownSession: the session is not live on this node.
	ErrUnknownSession = errors.New("unknown session")
	// ErrNotYourDelivery: the delivery exists and was made to someone else.
	ErrNotYourDelivery = errors.New("that delivery was made to another Expertise")
	// ErrNoSuchDestination: the delivery is unknown and the node/port given
	// is not one this credential's capability occupies in the graph.
	ErrNoSuchDestination = errors.New("no such destination for this credential in the graph")
)

// ResolveDelivery answers DeliveryRef for a party.
func (m *Manager) ResolveDelivery(ref DeliveryRef, who Party) (Resolved, error) {
	m.mu.RLock()
	sess, ok := m.sessions[ref.Session]
	m.mu.RUnlock()
	if !ok {
		return Resolved{}, ErrUnknownSession
	}
	return sess.resolveDelivery(ref, who)
}

func (s *Session) resolveDelivery(ref DeliveryRef, who Party) (Resolved, error) {
	out := Resolved{Session: s.ID, Graph: s.Graph.GraphID}

	// touch, not peek: a request naming a delivery is work continuing under
	// it, which is exactly what keeps an entry from being evicted.
	if d, ok := s.lineage.touch(ref.Delivery); ok {
		if !who.Operator {
			// A scoped credential may only speak for deliveries sent under
			// that same credential. Not "the same capability": two providers
			// of one capability are two parties.
			if d.token == "" || d.token != who.TokenID {
				return Resolved{}, ErrNotYourDelivery
			}
			out.Verified = true
		}
		out.Known = true
		out.Root, out.Cause = d.root, d.cause
		out.Priority = d.priority
		out.Deadline, out.RespondBy = d.validUntil, d.respondBy
		out.Cancelled = s.cancelled.has(d.root) || s.cancelled.has(d.cause)
		return out, nil
	}

	// Gone from the index, or never in it. The bounds are lost and said to
	// be. The priority is not: it belongs to the edge, and the graph is right
	// here. Where several edges feed the same port the lowest is taken — a
	// caller that cannot say which delivery it is answering does not get the
	// benefit of the doubt.
	found := false
	for _, dests := range s.routes {
		for _, d := range dests {
			if d.skill == nil || d.ref != ref.Node || d.port != ref.Port {
				continue
			}
			if !who.Operator && d.skill.Manifest.Capability != who.Capability {
				continue
			}
			if !found || d.priority < out.Priority {
				out.Priority = d.priority
			}
			found = true
		}
	}
	if !found {
		return Resolved{}, ErrNoSuchDestination
	}
	return out, nil
}

// CancelNotice says a chain's work is no longer wanted.
//
// Root and Cause are the two ways the executor itself names abandoned work:
// a client cancels a whole chain by its root, and a speculation that missed
// abandons the one delivery made on the wrong guess, by its cause. Both empty
// means the whole session is over.
type CancelNotice struct {
	Session string
	Root    string
	Cause   string
	Reason  string
}

// Reasons a CancelNotice carries.
const (
	CancelByClient      = "the chain was cancelled"
	CancelBySpeculation = "the speculation this work was started on was superseded"
	CancelBySessionEnd  = "the session ended"
)

// SetCancelObserver installs the function told whenever work is abandoned.
// It runs on the routing path, so it must not block: it is for flipping a
// flag or closing a channel, not for doing the cleanup.
func (m *Manager) SetCancelObserver(fn func(CancelNotice)) {
	if fn == nil {
		m.onCancel.Store(nil)
		return
	}
	m.onCancel.Store(&fn)
}

func (m *Manager) notifyCancel(n CancelNotice) {
	if fn := m.onCancel.Load(); fn != nil {
		(*fn)(n)
	}
}

// cancelObserver is the shape Manager stores; a pointer so a Session can share
// the Manager's slot and see an observer installed after it started.
type cancelObserver = atomic.Pointer[func(CancelNotice)]

func (s *Session) notifyCancel(root, cause, reason string) {
	if s.onCancel == nil {
		return
	}
	if fn := s.onCancel.Load(); fn != nil {
		(*fn)(CancelNotice{Session: s.ID, Root: root, Cause: cause, Reason: reason})
	}
}
