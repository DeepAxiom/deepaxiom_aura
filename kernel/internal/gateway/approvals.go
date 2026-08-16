package gateway

import (
	"encoding/json"
	"net/http"

	"aura/kernel/internal/approvals"
	"aura/kernel/internal/ledger"
)

// The approval surface: the questions the kernel is currently asking, and the
// way to answer one.
//
// These routes do not decide anything. The executor decides that an effect
// needs approval and refuses to deliver without one; all that lives here is a
// way for a human who is *not* holding the session socket to see the question
// and answer it. See internal/approvals for why that gap exists at all.
//
// Authentication is the node's ordinary bearer token — the same as every other
// control-plane route. That is the correct level: whoever can reach this can
// already register a graph, and an approval is a weaker capability than that.

func (g *Gateway) listApprovals(w http.ResponseWriter, _ *http.Request) {
	if g.Approvals == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no approval queue"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": g.Approvals.List()})
}

func (g *Gateway) resolveApproval(w http.ResponseWriter, r *http.Request) {
	if g.Approvals == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no approval queue"})
		return
	}
	id := r.PathValue("id")
	var body struct {
		// Approve is a pointer so that an omitted field is a bad request
		// rather than a silent denial. Answering a gate is the one place where
		// guessing what the caller meant is not acceptable.
		Approve *bool `json:"approve"`
		// Approval is the operator's signed statement (C4 v1.3), optional here
		// and forwarded unexamined. This route deliberately does not verify it:
		// the executor's gate is the only place that both can verify and will
		// seal, and a second check here would be a second implementation to
		// keep in step — one that, being on the permissive side of the gate,
		// could only ever disagree by letting something through.
		Approval *ledger.Approval `json:"approval,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "body must be {\"approve\": true|false}"})
		return
	}
	if body.Approve == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "\"approve\" is required and must be true or false"})
		return
	}
	if err := g.Approvals.Resolve(id, approvals.Answer{
		Approve: *body.Approve, Approval: body.Approval,
	}); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	signer := ""
	if body.Approval != nil {
		signer = body.Approval.Operator
	}
	g.Log.Info("approval answered", "id", id, "approved", *body.Approve, "signed_by", signer)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "approved": *body.Approve, "signed_by": signer})
}
