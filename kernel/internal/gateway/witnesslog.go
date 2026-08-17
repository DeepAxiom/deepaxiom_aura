package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/store"
)

// The witness's own log, served (C4 v1.4).
//
// Every route here is read-only and, on an open witness, unauthenticated. That
// is not a relaxation of the security model — it is the security model. A
// witness exists so that parties who do not trust each other can rely on the
// same anchor; a log only its own operator can read gives none of them anything
// to check. Publishing it is what converts "trust this witness" into "follow
// this witness and catch it if it lies".
//
// Nothing here discloses anything a node did not already choose to reveal. The
// records are heads and roots — sequence numbers, hashes, public keys and
// signatures. **No payload, no capability, no session, no operator.** A witness
// that leaked what its nodes were doing would be unusable by exactly the
// organisations that most need one, so it is built to have nothing to leak.

const maxWitnessLogPage = 1000

func (g *Gateway) witnessRequired(w http.ResponseWriter) bool {
	if g.Wit == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this node is not acting as a witness"})
		return false
	}
	return true
}

// witnessLogHead serves the witness's signed head (GET /v1/witness/head).
//
// The first call a monitor makes and the last thing it stores. Signed, so a
// monitor keeps evidence rather than a memory.
func (g *Gateway) witnessLogHead(w http.ResponseWriter, _ *http.Request) {
	if !g.witnessRequired(w) {
		return
	}
	writeJSON(w, http.StatusOK, g.Wit.LogHead())
}

// witnessLogEntries serves a page of the log (GET /v1/witness/log?from=&limit=).
func (g *Gateway) witnessLogEntries(w http.ResponseWriter, r *http.Request) {
	if !g.witnessRequired(w) {
		return
	}
	from, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > maxWitnessLogPage {
		limit = maxWitnessLogPage
	}
	entries, err := g.Wit.LogEntries(from, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if entries == nil {
		entries = []ledger.WitnessRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// witnessLogConsistency proves the log at one size is a prefix of it at another
// (GET /v1/witness/consistency?from=&to=).
//
// The single most important route for a monitor: it is what catches a witness
// that rewrote or dropped an entry it had already published.
func (g *Gateway) witnessLogConsistency(w http.ResponseWriter, r *http.Request) {
	if !g.witnessRequired(w) {
		return
	}
	// Absent and zero are different questions, and conflating them broke the
	// case a monitor actually starts from. `from=0` means "I have verified
	// nothing yet" — every tree extends the empty tree, so the honest answer is
	// an empty proof, not an error. Only a genuinely missing parameter is a bad
	// request.
	q := r.URL.Query()
	if !q.Has("from") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing ?from= (the size you already verified; 0 if none)"})
		return
	}
	from, err := strconv.ParseUint(q.Get("from"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "?from= must be a number"})
		return
	}
	to, _ := strconv.ParseUint(q.Get("to"), 10, 64)
	proof, err := g.Wit.LogConsistency(from, to)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "proof": proof})
}

// witnessLogInclusion proves one countersignature is really in the published
// log (GET /v1/witness/proof/{seq}).
//
// What a node presents when it wants to show a third party that its anchor was
// published rather than handed over privately.
func (g *Gateway) witnessLogInclusion(w http.ResponseWriter, r *http.Request) {
	if !g.witnessRequired(w) {
		return
	}
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "seq must be a number"})
		return
	}
	rec, proof, size, err := g.Wit.LogInclusion(seq)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entry": rec, "proof": proof, "log_size": size})
}

// ── following somebody else's witness ───────────────────────────────
//
// The monitor's memory. `aura witness audit` is only worth running because it
// compares against what it verified last time: a witness that publishes a head
// and later publishes a smaller or incompatible one is caught by whoever kept
// the earlier one, and by nobody else. These two routes are that keeping.
//
// Authenticated like any other control-plane route — this is *this* node's
// record of what it has checked, and letting a stranger overwrite it would let
// them erase the baseline an audit depends on.

// witnessSeen returns what this node last verified about a witness
// (GET /v1/witness/seen?key=…).
func (g *Gateway) witnessSeen(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing ?key="})
		return
	}
	row, ok, err := g.St.SeenWitnessHeadFor(key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"known": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"known": true,
		"head": ledger.LogHead{
			WitnessKey: row.WitnessKey, Size: row.Size, Root: row.Root, TS: row.TS,
		},
		"witness_url": row.WitnessURL,
	})
}

// witnessRecordSeen stores a verified head (POST /v1/witness/seen).
//
// The signature is re-checked here rather than taken on the CLI's word, for
// the same reason a countersignature is re-checked before it is stored: a
// baseline that was never verified is a baseline that proves nothing, and it
// would sit in the database looking exactly like one that was.
func (g *Gateway) witnessRecordSeen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WitnessURL string         `json:"witness_url"`
		Head       ledger.LogHead `json:"head"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `body must be {"witness_url": "…", "head": {…}}`})
		return
	}
	if err := ledger.VerifyLogHead(body.Head); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "refusing to record a head that does not verify: " + err.Error()})
		return
	}
	// Never move backwards. A monitor that could be talked into lowering its
	// own baseline could be talked into forgetting the very entries that would
	// convict a witness — so the record only ever advances.
	if prev, ok, err := g.St.SeenWitnessHeadFor(body.Head.WitnessKey); err == nil && ok {
		if body.Head.Size < prev.Size {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "refusing to lower the recorded baseline for this witness"})
			return
		}
	}
	if err := g.St.SaveSeenWitnessHead(store.SeenWitnessHead{
		WitnessKey: body.Head.WitnessKey, WitnessURL: body.WitnessURL,
		Size: body.Head.Size, Root: body.Head.Root, TS: time.Now().UnixMilli(),
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	g.Log.Info("witness head recorded", "witness", ledger.Fingerprint(body.Head.WitnessKey),
		"size", body.Head.Size)
	writeJSON(w, http.StatusOK, map[string]any{"recorded": true, "size": body.Head.Size})
}
