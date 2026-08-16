package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// The operator roster and the credential broker, as control-plane routes.
//
// Both sit behind the node's ordinary bearer token, and for both that is the
// right level — but for opposite reasons, which is worth stating because the
// asymmetry looks like an oversight otherwise.
//
// Enrolling an operator is a *privileged* act and the token is what authorizes
// it. That is not a weakening: whoever holds the token can already register a
// graph and change the policy file's neighbours, so a roster they could not
// edit would be security theatre. What the token cannot do is *forge an
// approval*, because that needs a private key the node has never held. The
// separation is the point — control of the node buys you the ability to say who
// may approve, and never the ability to say that someone did.
//
// Reading a secret, by contrast, is not available at this level at all. There
// is no route that returns a value: the only way one leaves this process is
// through the broker, against a sealed receipt, into the call that receipt
// authorized. Holding the node token lets you set a credential and see that it
// exists. It does not let you read it back.

func (g *Gateway) listOperators(w http.ResponseWriter, _ *http.Request) {
	if g.Mgr == nil || g.Mgr.Approvers() == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no operator roster"})
		return
	}
	rows := g.Mgr.Approvers().List()
	if rows == nil {
		rows = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"operators": rows})
}

func (g *Gateway) enrollOperator(w http.ResponseWriter, r *http.Request) {
	if g.Mgr == nil || g.Mgr.Approvers() == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no operator roster"})
		return
	}
	var body struct {
		ID     string `json:"id"`
		Pubkey string `json:"pubkey"`
		Name   string `json:"name,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `body must be {"id": "...", "pubkey": "<base64 ed25519>"}`})
		return
	}
	if err := g.Mgr.Approvers().Enroll(body.ID, body.Pubkey, body.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	g.Log.Info("operator enrolled", "id", body.ID)
	writeJSON(w, http.StatusOK, map[string]any{"id": body.ID, "enrolled": true})
}

func (g *Gateway) revokeOperator(w http.ResponseWriter, r *http.Request) {
	if g.Mgr == nil || g.Mgr.Approvers() == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no operator roster"})
		return
	}
	id := r.PathValue("id")
	ok, err := g.Mgr.Approvers().Revoke(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no enrolled operator " + id})
		return
	}
	g.Log.Info("operator revoked", "id", id)
	// Said explicitly because it is the question every operator asks next, and
	// the wrong guess ("their approvals are void now") is the dangerous one.
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "revoked": true,
		"note": "past approvals by this operator remain valid and verifiable"})
}

// ── secrets ─────────────────────────────────────────────────────────

func (g *Gateway) listSecrets(w http.ResponseWriter, _ *http.Request) {
	if g.Broker == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no credential broker"})
		return
	}
	names, err := g.Broker.Names()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": names})
}

func (g *Gateway) putSecret(w http.ResponseWriter, r *http.Request) {
	if g.Broker == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no credential broker"})
		return
	}
	var body struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `body must be {"name": "...", "value": "..."}`})
		return
	}
	if err := g.Broker.Set(body.Name, body.Value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// The name only. Logging the value at info level is how credentials end up
	// in a log aggregator forever.
	g.Log.Info("secret stored", "name", body.Name)
	writeJSON(w, http.StatusOK, map[string]any{"name": body.Name, "stored": true})
}

func (g *Gateway) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if g.Broker == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no credential broker"})
		return
	}
	name := r.PathValue("name")
	ok, err := g.Broker.Delete(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no secret " + name})
		return
	}
	g.Log.Info("secret deleted", "name", name)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "deleted": true})
}

// resolveSecret is the broker's one outward-facing route: a skill running as a
// separate process exchanges the receipt it was delivered for the credential
// that effect authorizes.
//
// The node token is necessary and nowhere near sufficient. Every real check is
// the receipt's — sealed by this node, for this capability, delivered not
// denied, and seconds old. A caller holding the token and no receipt gets
// nothing, which is the property that makes this worth building: it means a
// leaked node token does not become a leaked ERP credential.
func (g *Gateway) resolveSecret(w http.ResponseWriter, r *http.Request) {
	if g.Broker == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node has no credential broker"})
		return
	}
	var body struct {
		Receipt    string   `json:"receipt"`
		Capability string   `json:"capability"`
		Names      []string `json:"names"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `body must be {"receipt": "sha256:…", "capability": "motor.…", "names": ["…"]}`})
		return
	}
	grant, err := g.Broker.Resolve(strings.TrimSpace(body.Receipt), body.Capability, body.Names)
	if err != nil {
		// 403 rather than 400: the request was well-formed and was refused.
		// A caller retrying on 400 would be right to; on this, it would not.
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	g.Log.Info("credential released", "capability", grant.Capability,
		"effect", grant.Seq, "names", len(grant.Values))
	writeJSON(w, http.StatusOK, grant)
}
