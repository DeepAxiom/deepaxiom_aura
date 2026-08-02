package gateway

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"aura/kernel/internal/executor"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// testGatewayWithLedger builds a Gateway with a real ledger wired the way
// main.go wires one — same store the Gateway itself uses, so a caller sealing
// an effect and a caller reading /v1/ledger are looking at the same chain.
// Returns the data dir too, for the one test that has to tamper with the raw
// SQLite file to prove /v1/ledger/verify actually notices.
func testGatewayWithLedger(t *testing.T) (*Gateway, http.Handler, string) {
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
	ldg, err := ledger.Open(st, "node-test", keys)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}

	reg := registry.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{
		Node: &identity.Node{ID: "node-test", Mode: identity.ModeLocal},
		Reg:  reg, St: st,
		Mgr: executor.NewManager(reg, st, string(identity.ModeLocal), nil, ldg, log),
		Adm: NewAdmission(0), Ldg: ldg, Log: log,
	}
	return g, g.Handler(), dir
}

func sealTestEffect(t *testing.T, ldg *ledger.Ledger, capability string) string {
	t.Helper()
	receipt, err := ldg.Seal(ledger.SealRequest{
		Session: "sess-1", Envelope: "ENV-1", Cause: "ENV-0",
		Actor: "acme/motor/writer@1.0.0", Capability: capability,
		Decision: spec.DecisionAllow, Outcome: spec.OutcomeDelivered,
		Policy: "sha256:policyhash", Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return receipt
}

// --- /healthz --------------------------------------------------------------

func TestHealthzOmitsLedgerWhenNoneIsWired(t *testing.T) {
	_, h := testGateway(t) // the plain fixture: Ldg is nil
	code, body := do(t, h, "GET", "/healthz", nil)
	if code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	if _, ok := body["ledger"]; ok {
		t.Fatal("a node with no ledger reported one in /healthz")
	}
}

func TestHealthzReportsLedgerSummary(t *testing.T) {
	g, h, _ := testGatewayWithLedger(t)
	sealTestEffect(t, g.Ldg, "motor.erp.write")
	sealTestEffect(t, g.Ldg, "motor.tts.speak")

	code, body := do(t, h, "GET", "/healthz", nil)
	if code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	ledgerField, ok := body["ledger"].(map[string]any)
	if !ok {
		t.Fatalf("no ledger summary in /healthz: %v", body)
	}
	if n, _ := ledgerField["entries"].(float64); n != 2 {
		t.Errorf("ledger.entries = %v, want 2", ledgerField["entries"])
	}
	if ledgerField["node_pubkey"] == "" || ledgerField["node_pubkey"] == nil {
		t.Error("ledger summary carries no node public key")
	}
}

// --- GET /v1/ledger ----------------------------------------------------------

func TestListLedgerReturns404WhenNotWired(t *testing.T) {
	_, h := testGateway(t)
	code, _ := do(t, h, "GET", "/v1/ledger", nil)
	if code != 404 {
		t.Fatalf("status = %d, want 404 for a node with no ledger", code)
	}
}

func TestListLedgerReturnsSealedEntries(t *testing.T) {
	g, h, _ := testGatewayWithLedger(t)
	sealTestEffect(t, g.Ldg, "motor.erp.write")
	sealTestEffect(t, g.Ldg, "motor.tts.speak")

	code, body := do(t, h, "GET", "/v1/ledger", nil)
	if code != 200 {
		t.Fatalf("status = %d, body %v", code, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if total, _ := body["total"].(float64); total != 2 {
		t.Errorf("total = %v, want 2", body["total"])
	}
}

func TestListLedgerPaginates(t *testing.T) {
	g, h, _ := testGatewayWithLedger(t)
	for i := 0; i < 5; i++ {
		sealTestEffect(t, g.Ldg, "motor.erp.write")
	}

	code, body := do(t, h, "GET", "/v1/ledger?limit=2", nil)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("got %d entries with limit=2, want 2", len(entries))
	}
	if total, _ := body["total"].(float64); total != 5 {
		t.Errorf("total = %v, want 5 (the full count, not just this page)", body["total"])
	}

	code, body = do(t, h, "GET", "/v1/ledger?from_seq=4", nil)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	entries, _ = body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("got %d entries from seq 4, want 2 (seq 4 and 5)", len(entries))
	}
}

func TestListLedgerOnAnEmptyLedgerIsAnEmptyPage(t *testing.T) {
	_, h, _ := testGatewayWithLedger(t)
	code, body := do(t, h, "GET", "/v1/ledger", nil)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if total, _ := body["total"].(float64); total != 0 {
		t.Errorf("total = %v, want 0", body["total"])
	}
}

// A public-facing listing endpoint requires the node token exactly like
// every other route on the control surface — the ledger is not an exception.
func TestListLedgerRequiresTheToken(t *testing.T) {
	g, _, _ := testGatewayWithLedger(t)
	g.Auth = &Auth{Token: "s3cret"}
	h := g.Handler() // rebuilt: Handler() captures Auth at call time
	sealTestEffect(t, g.Ldg, "motor.erp.write")

	req := httptest.NewRequest(http.MethodGet, "/v1/ledger", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a token", rec.Code)
	}
}

// --- GET /v1/ledger/verify ----------------------------------------------------

func TestVerifyLedgerReturns404WhenNotWired(t *testing.T) {
	_, h := testGateway(t)
	code, _ := do(t, h, "GET", "/v1/ledger/verify", nil)
	if code != 404 {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestVerifyLedgerReportsSoundOnAnUntamperedChain(t *testing.T) {
	g, h, _ := testGatewayWithLedger(t)
	sealTestEffect(t, g.Ldg, "motor.erp.write")
	sealTestEffect(t, g.Ldg, "motor.tts.speak")
	if err := g.Ldg.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	code, body := do(t, h, "GET", "/v1/ledger/verify", nil)
	if code != 200 {
		t.Fatalf("status = %d, body %v", code, body)
	}
	if sound, _ := body["sound"].(bool); !sound {
		t.Fatalf("an untampered ledger reported unsound: %v", body)
	}
}

// The one test in this file that reaches past the Gateway/Store API to edit
// the SQLite file directly — simulating the actual threat model this endpoint
// exists to catch. See kernel/internal/ledger's own test suite for the
// exhaustive version of this; here the point is only that the HTTP surface
// reports what Verify found, faithfully and with a status code a monitor can
// alert on without parsing the body.
func TestVerifyLedgerReturns409WhenTampered(t *testing.T) {
	g, h, dir := testGatewayWithLedger(t)
	sealTestEffect(t, g.Ldg, "motor.erp.write")
	sealTestEffect(t, g.Ldg, "motor.tts.speak")

	dsn := filepath.Join(dir, "kernel.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE ledger_entries SET entry = REPLACE(entry, 'motor.erp.write', 'motor.payments.send') WHERE seq = 1`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	code, body := do(t, h, "GET", "/v1/ledger/verify", nil)
	if code != 409 {
		t.Fatalf("status = %d, want 409 for a tampered ledger", code)
	}
	if sound, _ := body["sound"].(bool); sound {
		t.Fatal("a tampered ledger reported itself sound")
	}
	report, _ := body["report"].(map[string]any)
	if intact, _ := report["chain_intact"].(bool); intact {
		t.Fatal("report.chain_intact is true despite the tamper")
	}
}

// --- GET /v1/sessions/{id}/ledger --------------------------------------------
// (Phase 2, `aura undo`: what a session-mode undo walks backwards.)

func TestSessionLedgerReturns404WhenNotWired(t *testing.T) {
	_, h := testGateway(t)
	code, _ := do(t, h, "GET", "/v1/sessions/sess-1/ledger", nil)
	if code != 404 {
		t.Fatalf("status = %d, want 404 for a node with no ledger", code)
	}
}

func TestSessionLedgerFiltersToOneSession(t *testing.T) {
	g, h, _ := testGatewayWithLedger(t)
	sealTestEffect(t, g.Ldg, "motor.erp.write") // session "sess-1", per sealTestEffect

	code, body := do(t, h, "GET", "/v1/sessions/sess-1/ledger", nil)
	if code != 200 {
		t.Fatalf("status = %d, body %v", code, body)
	}
	if got, _ := body["session"].(string); got != "sess-1" {
		t.Errorf("session = %q, want sess-1", got)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}

	code, body = do(t, h, "GET", "/v1/sessions/no-such-session/ledger", nil)
	if code != 200 {
		t.Fatalf("status = %d, want 200 (an empty page, not an error)", code)
	}
	entries, _ = body["entries"].([]any)
	if len(entries) != 0 {
		t.Fatalf("got %d entries for an unknown session, want 0", len(entries))
	}
}

// --- GET /v1/ledger/entries/{hash} -------------------------------------------
// (Phase 2, `aura undo <receipt>`: single-effect mode, and the lookup
// executor.validateUndo needs before an undo session is ever built.)

func TestLedgerEntryReturns404WhenNotWired(t *testing.T) {
	_, h := testGateway(t)
	code, _ := do(t, h, "GET", "/v1/ledger/entries/sha256:doesnotmatter", nil)
	if code != 404 {
		t.Fatalf("status = %d, want 404 for a node with no ledger", code)
	}
}

func TestLedgerEntryReturnsTheEntryByReceipt(t *testing.T) {
	g, h, _ := testGatewayWithLedger(t)
	receipt := sealTestEffect(t, g.Ldg, "motor.erp.write")

	code, body := do(t, h, "GET", "/v1/ledger/entries/"+receipt, nil)
	if code != 200 {
		t.Fatalf("status = %d, body %v", code, body)
	}
	if got, _ := body["capability"].(string); got != "motor.erp.write" {
		t.Errorf("capability = %q, want motor.erp.write", got)
	}
}

func TestLedgerEntryReturns404ForAnUnknownReceipt(t *testing.T) {
	g, h, _ := testGatewayWithLedger(t)
	sealTestEffect(t, g.Ldg, "motor.erp.write")

	code, _ := do(t, h, "GET", "/v1/ledger/entries/sha256:not-a-real-receipt", nil)
	if code != 404 {
		t.Fatalf("status = %d, want 404 for a receipt that was never sealed", code)
	}
}
