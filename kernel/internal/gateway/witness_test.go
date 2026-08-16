package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"aura/kernel/internal/executor"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// The witnessing handshake over its real HTTP surface, between two separately
// constructed nodes.
//
// The ledger package proves the protocol; this proves the wiring — that the
// endpoints exist, speak the shapes the CLI sends, and that a node can
// actually anchor its history against a peer without either of them being
// special.

// namedGateway builds a Gateway that is both a node with its own ledger and a
// witness for others, the way main.go does.
func namedGateway(t *testing.T, id string) (*Gateway, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	keys, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	ldg, err := ledger.Open(st, id, keys)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}

	reg := registry.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{
		Node: &identity.Node{ID: id, Mode: identity.ModeLocal},
		Reg:  reg, St: st,
		Mgr: executor.NewManager(reg, st, string(identity.ModeLocal), nil, ldg, log),
		Adm: NewAdmission(0), Ldg: ldg, Wit: ledger.NewWitness(st, keys), Log: log,
	}
	return g, g.Handler()
}

func getJSONFrom(t *testing.T, h http.Handler, path string, out any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if out != nil && rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec.Code
}

func postJSONTo(t *testing.T, h http.Handler, path string, body any, out any) int {
	t.Helper()
	blob, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, bytes.NewReader(blob))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec.Code
}

// anchor runs the full handshake the `aura witness` CLI runs, over HTTP.
func anchor(t *testing.T, node *Gateway, nodeH, witnessH http.Handler) (ledger.Statement, int) {
	t.Helper()
	var seen struct {
		Seq uint64 `json:"seq"`
	}
	if code := getJSONFrom(t, witnessH,
		"/v1/ledger/witness/last-seen?node="+node.Node.ID, &seen); code != 200 {
		t.Fatalf("last-seen returned %d", code)
	}

	var stmt ledger.Statement
	path := "/v1/ledger/statement?from_seq=" + itoa(seen.Seq)
	if code := getJSONFrom(t, nodeH, path, &stmt); code != 200 {
		t.Fatalf("statement returned %d", code)
	}

	var cs ledger.Countersignature
	code := postJSONTo(t, witnessH, "/v1/ledger/witness", stmt, &cs)
	if code != 200 {
		return stmt, code
	}

	var rec map[string]any
	if c := postJSONTo(t, nodeH, "/v1/ledger/witness/record", map[string]any{
		"seq": stmt.Seq, "merkle_root": stmt.MerkleRoot,
		"witness_url": "http://witness.test", "countersignature": cs,
	}, &rec); c != 200 {
		t.Fatalf("recording the countersignature returned %d: %v", c, rec)
	}
	return stmt, 200
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestWitnessHandshakeOverHTTP(t *testing.T) {
	node, nodeH := namedGateway(t, "node-a")
	_, witnessH := namedGateway(t, "node-b")

	for i := 0; i < 4; i++ {
		sealTestEffect(t, node.Ldg, "motor.erp.invoice.create")
	}

	stmt, code := anchor(t, node, nodeH, witnessH)
	if code != 200 {
		t.Fatalf("witness refused an honest first contact: %d", code)
	}
	if stmt.Seq != 4 || stmt.MerkleRoot == "" {
		t.Fatalf("statement = seq %d root %q", stmt.Seq, stmt.MerkleRoot)
	}

	// The node's own verification now reports the anchor.
	var verify struct {
		Sound  bool          `json:"sound"`
		Report ledger.Report `json:"report"`
	}
	if code := getJSONFrom(t, nodeH, "/v1/ledger/verify", &verify); code != 200 {
		t.Fatalf("verify returned %d", code)
	}
	if !verify.Sound {
		t.Fatalf("a witnessed ledger reported unsound: %+v", verify.Report)
	}
	if verify.Report.WitnessesValid != 1 {
		t.Fatalf("verify reports %d/%d valid witnesses, want 1",
			verify.Report.WitnessesValid, verify.Report.Witnesses)
	}
	if verify.Report.MerkleRoot == "" {
		t.Fatal("verify reports no merkle root")
	}

	// Growing honestly and re-anchoring must keep working.
	for i := 0; i < 5; i++ {
		sealTestEffect(t, node.Ldg, "motor.erp.invoice.create")
	}
	stmt2, code := anchor(t, node, nodeH, witnessH)
	if code != 200 {
		t.Fatalf("witness refused an honest extension: %d", code)
	}
	if stmt2.Seq != 9 {
		t.Fatalf("second statement covers %d entries, want 9", stmt2.Seq)
	}
	if stmt2.FromSeq != 4 || len(stmt2.Consistency) == 0 {
		t.Fatalf("expected a consistency proof anchored at 4, got from=%d len=%d",
			stmt2.FromSeq, len(stmt2.Consistency))
	}
}

// A witness must refuse a statement whose signature does not verify, even
// though the request itself is perfectly well-formed.
func TestWitnessEndpointRefusesAForgedStatement(t *testing.T) {
	node, nodeH := namedGateway(t, "node-a")
	_, witnessH := namedGateway(t, "node-b")
	sealTestEffect(t, node.Ldg, "motor.erp.invoice.create")

	var stmt ledger.Statement
	if code := getJSONFrom(t, nodeH, "/v1/ledger/statement?from_seq=0", &stmt); code != 200 {
		t.Fatalf("statement returned %d", code)
	}
	stmt.Signature = "Zm9yZ2Vk"

	var body map[string]string
	if code := postJSONTo(t, witnessH, "/v1/ledger/witness", stmt, &body); code != 409 {
		t.Fatalf("witness answered %d for a forged statement, want 409", code)
	}
	if body["error"] == "" {
		t.Fatal("a refusal carried no explanation")
	}
}

// The receipt endpoint must produce a document that verifies with nothing but
// itself — checked here by verifying it after it has been through HTTP and
// JSON, which is how a real consumer would receive it.
func TestReceiptEndpointProducesStandaloneEvidence(t *testing.T) {
	node, nodeH := namedGateway(t, "node-a")
	_, witnessH := namedGateway(t, "node-b")

	var target string
	for i := 0; i < 5; i++ {
		target = sealTestEffect(t, node.Ldg, "motor.erp.invoice.create")
	}
	if _, code := anchor(t, node, nodeH, witnessH); code != 200 {
		t.Fatalf("anchoring failed: %d", code)
	}

	var r ledger.Receipt
	if code := getJSONFrom(t, nodeH, "/v1/ledger/receipt/"+target, &r); code != 200 {
		t.Fatalf("receipt endpoint returned %d", code)
	}
	rep := ledger.VerifyReceipt(r)
	if !rep.Sound() {
		t.Fatalf("a receipt served over HTTP did not verify: %+v", rep)
	}
	if rep.WitnessesValid != 1 {
		t.Fatalf("receipt carries %d/%d valid witnesses, want 1",
			rep.WitnessesValid, rep.Witnesses)
	}
}

func TestLedgerHeadEndpoint(t *testing.T) {
	node, nodeH := namedGateway(t, "node-a")

	var head ledger.Head
	if code := getJSONFrom(t, nodeH, "/v1/ledger/head", &head); code != 200 {
		t.Fatalf("head returned %d", code)
	}
	// An empty ledger still has a real, signable head rather than a special case.
	if head.Seq != 0 || head.MerkleRoot == "" {
		t.Fatalf("empty ledger head = %+v; want seq 0 with the empty-tree root", head)
	}

	sealTestEffect(t, node.Ldg, "motor.erp.invoice.create")
	if code := getJSONFrom(t, nodeH, "/v1/ledger/head", &head); code != 200 {
		t.Fatalf("head returned %d", code)
	}
	if head.Seq != 1 || head.Node != "node-a" || head.Pubkey == "" {
		t.Fatalf("head after one effect = %+v", head)
	}
}
