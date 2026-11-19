package gateway

import (
	"errors"
	"net/http"
	"sort"

	"aura/kernel/internal/executor"
	"aura/kernel/internal/infer"
)

// The join between the inference plane and the executor.
//
// The two packages do not import each other, on purpose. The plane knows what
// an OpenAI-shaped request looks like and nothing about graphs; the executor
// knows what a delivery is and nothing about models. What each needs from the
// other is small enough to state as two functions, and this file is where they
// are stated: a question the plane asks ("whose work is this"), and a notice
// the executor gives ("nobody is waiting for that any more").

// inference serves the node's OpenAI-compatible routes.
func (g *Gateway) inference(w http.ResponseWriter, r *http.Request) {
	if g.Infer == nil {
		// Said in the shape an OpenAI client shows, because whoever reads this
		// is reading an Expertise's log and needs to know it is a setting, not
		// an outage.
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"type": "aura_inference", "code": "no_engine",
			"message": "this node has no inference engine configured. Start it with " +
				"--inference-url <the engine's OpenAI-compatible base URL>, or point the " +
				"Expertise at the engine directly",
		}})
		return
	}
	p := PrincipalOf(r)
	g.Infer.Serve(w, r, infer.Who{
		Operator: p.IsOperator(), TokenID: p.TokenID, Capability: p.Capability,
	}, r.PathValue("route"))
}

// InferenceResolver is how the plane asks the executor about a delivery.
func InferenceResolver(mgr *executor.Manager) infer.Resolver {
	return func(ref infer.Ref, who infer.Who) (infer.Resolved, error) {
		res, err := mgr.ResolveDelivery(
			executor.DeliveryRef{Session: ref.Session, Delivery: ref.Delivery, Node: ref.Node, Port: ref.Port},
			executor.Party{Operator: who.Operator, TokenID: who.TokenID, Capability: who.Capability})
		switch {
		case errors.Is(err, executor.ErrNotYourDelivery):
			return infer.Resolved{}, infer.ErrForeign
		case err != nil:
			// The session is gone, or the node and port named are not this
			// caller's. Either way the request claims a place in a graph that
			// the executor cannot give it.
			return infer.Resolved{}, errors.Join(infer.ErrUnplaced, err)
		}
		return infer.Resolved{
			Session: res.Session, Graph: res.Graph, Root: res.Root, Cause: res.Cause,
			Priority: res.Priority, Deadline: res.Deadline,
			Known: res.Known, Verified: res.Verified, Cancelled: res.Cancelled,
		}, nil
	}
}

// ConnectInference has the executor tell the plane when work is abandoned.
//
// Before this, cancelling a chain sent a message to each Expertise working on
// it and relied on each one to notice and stop its model. Most do, at their
// next token; one blocked waiting for a slot does not, and neither does one
// that never checks. With the plane in the path the node closes the request at
// the engine itself, and what the Expertise does about the message stops
// mattering to whether the model is released.
func ConnectInference(mgr *executor.Manager, plane *infer.Plane) {
	mgr.SetCancelObserver(func(n executor.CancelNotice) {
		kind := infer.StopChain
		switch n.Reason {
		case executor.CancelBySpeculation:
			kind = infer.StopSpeculation
		case executor.CancelBySessionEnd:
			kind = infer.StopSession
		}
		plane.Cancel(n.Session, n.Root, n.Cause, kind, n.Reason)
	})
}

// sortedKeys orders a set of labels, so two scrapes of the same counters
// print the same lines in the same order.
func sortedKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
