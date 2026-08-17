package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"aura/kernel/internal/ledger"
)

// The effect ledger's read surface (C4). Sealing itself happens inside the
// executor, at the one point every effect passes through (see
// executor.Session.sealEffect) — nothing here writes to the ledger, which is
// deliberate: an HTTP client can look, never seal, the same asymmetry the
// rest of the kernel already keeps between what a graph can request and what
// only the executor is trusted to decide.

const (
	defaultLedgerPage = 100
	maxLedgerPage     = 1000
)

// listLedger returns a page of sealed entries (GET /v1/ledger?from_seq=&limit=).
//
// `aura verify` does not use this — it reads the store directly and
// unbounded, because recomputing a chain needs every link. This endpoint is
// for a human or a dashboard paging through recent history, so it is capped
// even when a caller asks for more.
func (g *Gateway) listLedger(w http.ResponseWriter, r *http.Request) {
	if g.Ldg == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no effect ledger"})
		return
	}
	fromSeq := parseSeqParam(r.URL.Query().Get("from_seq"), 1)
	limit := int(parseSeqParam(r.URL.Query().Get("limit"), defaultLedgerPage))
	if limit <= 0 || limit > maxLedgerPage {
		limit = maxLedgerPage
	}

	entries, err := g.St.LedgerEntries(fromSeq, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	sum, err := g.Ldg.Summarize()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"entries": entries, "returned": len(entries), "total": sum.Entries,
	})
}

// sessionLedger returns one session's sealed entries in seq order (GET
// /v1/sessions/{id}/ledger) — what `aura undo <session>` walks backwards.
// Unbounded, like `aura verify`'s read and unlike listLedger's page: a
// session's own effects are already a small, bounded slice of the whole
// ledger, so there is no flood risk in returning all of them.
func (g *Gateway) sessionLedger(w http.ResponseWriter, r *http.Request) {
	if g.Ldg == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no effect ledger"})
		return
	}
	session := r.PathValue("id")
	entries, err := g.St.LedgerEntriesBySession(session)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"session": session, "entries": entries})
}

// ledgerEntry returns one sealed entry by its own receipt (GET
// /v1/ledger/entries/{hash}) — what `aura undo <receipt>` (single-effect
// mode) looks up, and what a session-mode undo uses to check whether an
// entry it is about to reverse was already undone by an earlier run.
func (g *Gateway) ledgerEntry(w http.ResponseWriter, r *http.Request) {
	if g.Ldg == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no effect ledger"})
		return
	}
	entry, err := g.St.LedgerEntryByHash(r.PathValue("hash"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(entry)
}

func parseSeqParam(v string, fallback uint64) uint64 {
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

// verifyLedger recomputes the whole chain and checks every checkpoint
// signature (GET /v1/ledger/verify) — the same function `aura verify` calls
// offline, so a verification done against a running node and one done
// against a cold data directory can never quietly disagree.
func (g *Gateway) verifyLedger(w http.ResponseWriter, _ *http.Request) {
	if g.Ldg == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no effect ledger"})
		return
	}
	report, err := ledger.Verify(g.St, g.Ldg.NodePublicKey())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	code := 200
	if !report.Sound() {
		// Not a client error — the request succeeded and answered truthfully.
		// 200 would bury the one fact this endpoint exists to surface in a
		// body a casual caller might not parse; 409 makes "this ledger is not
		// trustworthy" visible in the status line and in any uptime monitor
		// watching this URL.
		code = 409
	}
	writeJSON(w, code, map[string]any{
		"sound": report.Sound(), "report": report,
	})
}

// ledgerHead returns this node's current position (GET /v1/ledger/head):
// how many effects it has sealed, the linear chain head, and the Merkle tree
// head over all of them.
//
// Cheap and read-only, and the first call in the witnessing handshake: a
// witness asks for the head before deciding what consistency proof to demand.
func (g *Gateway) ledgerHead(w http.ResponseWriter, _ *http.Request) {
	if g.Ldg == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no effect ledger"})
		return
	}
	writeJSON(w, 200, g.Ldg.Head())
}

// ledgerStatement builds what this node should present to a witness that last
// saw it at ?from_seq= (GET /v1/ledger/statement).
//
// Read-only despite doing real work: it may force a checkpoint so the witness
// always receives a head this node has committed to under its own key. That
// is a write to the ledger's checkpoint table, not to its entries — the
// asymmetry this file's header describes still holds, because no caller can
// cause an *effect* to be sealed.
func (g *Gateway) ledgerStatement(w http.ResponseWriter, r *http.Request) {
	if g.Ldg == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no effect ledger"})
		return
	}
	stmt, err := g.Ldg.Statement(parseSeqParam(r.URL.Query().Get("from_seq"), 0))
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, stmt)
}

// ledgerWitness is this node acting as a witness for another (POST
// /v1/ledger/witness).
//
// The body is the other node's Statement; the response is a countersignature,
// or a refusal. A refusal here is the whole point of the endpoint and is
// returned as 409 rather than 400: the request was well-formed, and what
// failed is the claim it made about history. An uptime check pointed at a
// witness will see those 409s, which is the correct thing for it to see.
func (g *Gateway) ledgerWitness(w http.ResponseWriter, r *http.Request) {
	if g.Wit == nil {
		writeJSON(w, 404, map[string]string{
			"error": "this node is not acting as a witness (no identity keypair)"})
		return
	}
	var stmt ledger.Statement
	if err := json.NewDecoder(io.LimitReader(r.Body, maxStatementBytes)).Decode(&stmt); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid statement: " + err.Error()})
		return
	}
	cs, err := g.Wit.Countersign(stmt)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, cs)
}

// ledgerWitnessLastSeen answers "how far have you already vouched for this
// node?" (GET /v1/ledger/witness/last-seen?node=…).
//
// A node asks this before building a statement, so it knows which consistency
// proof to construct. Answering 0 for a node never seen is the first-contact
// case, not an error — a witness with no prior record has nothing to check
// against and takes the presented head as its baseline.
func (g *Gateway) ledgerWitnessLastSeen(w http.ResponseWriter, r *http.Request) {
	if g.Wit == nil {
		writeJSON(w, 404, map[string]string{"error": "this node is not acting as a witness"})
		return
	}
	node := r.URL.Query().Get("node")
	if node == "" {
		writeJSON(w, 400, map[string]string{"error": "missing ?node="})
		return
	}
	// Signed since C4 v1.4, and this is the change that makes a shared witness
	// worth relying on. An unsigned integer here let a witness tell the node
	// one thing and an auditor another, with neither able to prove it: two
	// rumours. Signed, the same two answers are two statements over the same
	// key that cannot both be true — a split view stops being undetectable and
	// becomes self-incriminating.
	//
	// `node` and `seq` stay at the top level so a node built against the
	// unsigned shape keeps working unchanged; everything else is additive.
	stmt, err := g.Wit.LastSeenSigned(node)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"node": stmt.Node, "seq": stmt.Seq, "statement": stmt,
	})
}

// ledgerRecordWitness stores a countersignature this node collected about its
// own ledger (POST /v1/ledger/witness/record).
//
// The node re-verifies the signature before storing it — see
// ledger.RecordCountersignature. A countersignature accepted on the CLI's
// word alone would let a broken or hostile witness plant a row that makes
// `aura verify` report an anchor that does not exist.
func (g *Gateway) ledgerRecordWitness(w http.ResponseWriter, r *http.Request) {
	if g.Ldg == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no effect ledger"})
		return
	}
	var body struct {
		Seq              uint64                  `json:"seq"`
		MerkleRoot       string                  `json:"merkle_root"`
		WitnessURL       string                  `json:"witness_url"`
		Countersignature ledger.Countersignature `json:"countersignature"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxStatementBytes)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if err := g.Ldg.RecordCountersignature(
		body.Seq, body.MerkleRoot, body.WitnessURL, body.Countersignature); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"recorded": true, "seq": body.Seq})
}

// maxStatementBytes bounds a witness request. A statement is a head plus a
// consistency proof: log2(n) hashes, so even a billion-entry ledger needs
// about 30 of them. The cap is generous by orders of magnitude and still
// stops an unauthenticated-shaped endpoint from being a memory sink.
const maxStatementBytes = 1 << 20

// ledgerReceipt builds the portable evidence document for one sealed effect
// (GET /v1/ledger/receipt/{hash}).
//
// What comes back is self-contained: entry, inclusion proof, signed head,
// witness countersignatures and the C5 attestations the entry cites. The
// caller can verify it later with no node, no database and no network — see
// ledger.VerifyReceipt.
func (g *Gateway) ledgerReceipt(w http.ResponseWriter, r *http.Request) {
	if g.Ldg == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no effect ledger"})
		return
	}
	receipt, err := ledger.BuildReceipt(g.St, r.PathValue("hash"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, receipt)
}

// ledgerBundle assembles the audit bundle for one session
// (GET /v1/sessions/{id}/bundle).
//
// The trajectory, a standalone receipt per sealed effect, and the model
// configurations behind them, in one document that verifies with no database,
// no node and no network — see ledger.VerifyBundle.
func (g *Gateway) ledgerBundle(w http.ResponseWriter, r *http.Request) {
	bundle, err := ledger.BuildBundle(g.St, r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, bundle)
}

// ledgerAttestation returns one C5 inference attestation by content address
// (GET /v1/ledger/attestations/{hash}) — what a model claimed about itself,
// re-checked against its hash on the way out.
func (g *Gateway) ledgerAttestation(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if _, err := ledger.LoadAttestation(g.St, hash); err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	raw, err := g.St.Attestation(hash)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}
