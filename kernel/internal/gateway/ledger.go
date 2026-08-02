package gateway

import (
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
