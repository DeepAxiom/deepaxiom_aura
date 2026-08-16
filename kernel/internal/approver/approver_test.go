package approver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return priv, base64.StdEncoding.EncodeToString(pub)
}

func sign(t *testing.T, id string, priv ed25519.PrivateKey, node, sess, env, decision string) *ledger.Approval {
	t.Helper()
	a, err := ledger.SignApproval(id, priv, node, sess, env, decision, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return a
}

func TestCheckAcceptsAnEnrolledOperator(t *testing.T) {
	r, err := Load(testStore(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	priv, pub := newKey(t)
	if err := r.Enroll("grace", pub, "Grace Hopper"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	a := sign(t, "grace", priv, "node-a", "sess-1", "env-1", ledger.ApprovalApprove)
	if err := r.Check(a, "node-a", "sess-1", "env-1"); err != nil {
		t.Fatalf("an enrolled operator's own signature was refused: %v", err)
	}
}

// The impersonation attack, and the reason Check exists at all rather than
// Verify being enough: anyone can generate a keypair and sign as "grace". The
// signature is internally valid. What stops it is that the node enrolled a
// different key under that name.
func TestCheckRefusesAValidSignatureFromAnUnenrolledKey(t *testing.T) {
	r, _ := Load(testStore(t))
	_, realPub := newKey(t)
	if err := r.Enroll("grace", realPub, ""); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	attacker, _ := newKey(t)
	forged := sign(t, "grace", attacker, "node-a", "sess-1", "env-1", ledger.ApprovalApprove)

	if err := forged.Verify("node-a", "sess-1"); err != nil {
		t.Fatalf("setup: the forgery should be internally consistent: %v", err)
	}
	err := r.Check(forged, "node-a", "sess-1", "env-1")
	if err == nil {
		t.Fatal("an approval signed by a key this node never enrolled was accepted as grace")
	}
	if !strings.Contains(err.Error(), "not enrolled") {
		t.Errorf("the refusal should say the key is not enrolled for them, got: %v", err)
	}
}

func TestCheckRefusesAnUnknownOperator(t *testing.T) {
	r, _ := Load(testStore(t))
	priv, _ := newKey(t)
	a := sign(t, "mallory", priv, "node-a", "sess-1", "env-1", ledger.ApprovalApprove)
	if err := r.Check(a, "node-a", "sess-1", "env-1"); err == nil {
		t.Fatal("an approval from someone never enrolled was accepted")
	}
}

func TestCheckRefusesAnApprovalForAnotherDelivery(t *testing.T) {
	r, _ := Load(testStore(t))
	priv, pub := newKey(t)
	_ = r.Enroll("grace", pub, "")
	a := sign(t, "grace", priv, "node-a", "sess-1", "env-1", ledger.ApprovalApprove)

	// Same operator, same session, real signature — but it answers a different
	// held delivery. Within one session that is the difference between
	// approving a $5 refund and approving a $50,000 one.
	if err := r.Check(a, "node-a", "sess-1", "env-2"); err == nil {
		t.Fatal("an approval for one delivery was accepted for another")
	}
}

// Revocation is forward-only. This is the property that keeps an audit trail
// stable: if revoking someone invalidated their past approvals, the ledger
// would say something different about last year every time HR did.
func TestRevocationStopsFutureApprovalsAndLeavesPastOnesValid(t *testing.T) {
	st := testStore(t)
	r, _ := Load(st)
	priv, pub := newKey(t)
	_ = r.Enroll("grace", pub, "")

	past := sign(t, "grace", priv, "node-a", "sess-1", "env-1", ledger.ApprovalApprove)
	if err := r.Check(past, "node-a", "sess-1", "env-1"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ok, err := r.Revoke("grace")
	if err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}

	future := sign(t, "grace", priv, "node-a", "sess-2", "env-2", ledger.ApprovalApprove)
	if err := r.Check(future, "node-a", "sess-2", "env-2"); err == nil {
		t.Error("a revoked operator could still approve")
	}

	// The sealed evidence is untouched: verification of a past approval never
	// consults the roster, so it cannot be changed by editing the roster.
	if err := past.Verify("node-a", "sess-1"); err != nil {
		t.Errorf("revoking an operator invalidated an approval they had already given: %v", err)
	}
}

func TestReEnrollingClearsRevocation(t *testing.T) {
	r, _ := Load(testStore(t))
	priv, pub := newKey(t)
	_ = r.Enroll("grace", pub, "")
	if _, err := r.Revoke("grace"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := r.Enroll("grace", pub, ""); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	a := sign(t, "grace", priv, "node-a", "sess-1", "env-1", ledger.ApprovalApprove)
	if err := r.Check(a, "node-a", "sess-1", "env-1"); err != nil {
		t.Errorf("a re-enrolled operator still cannot approve: %v", err)
	}
}

// One key, one identity. Two names for the same key would make a sealed
// approval ambiguous about who gave it, which defeats the whole point.
func TestOneKeyCannotBeTwoOperators(t *testing.T) {
	r, _ := Load(testStore(t))
	_, pub := newKey(t)
	if err := r.Enroll("grace", pub, ""); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := r.Enroll("alan", pub, ""); err == nil {
		t.Fatal("the same key was enrolled under two operator names")
	}
}

func TestEnrollValidatesTheKey(t *testing.T) {
	r, _ := Load(testStore(t))
	if err := r.Enroll("", "", ""); err == nil {
		t.Error("enrolled an operator with no id")
	}
	if err := r.Enroll("grace", "not base64!!", ""); err == nil {
		t.Error("enrolled a key that is not base64")
	}
	if err := r.Enroll("grace", base64.StdEncoding.EncodeToString([]byte("short")), ""); err == nil {
		t.Error("enrolled a key of the wrong length")
	}
}

func TestEmptyRosterIsReportedAndSurvivesRevocation(t *testing.T) {
	r, _ := Load(testStore(t))
	if !r.Empty() {
		t.Fatal("a fresh roster should be empty")
	}
	_, pub := newKey(t)
	_ = r.Enroll("grace", pub, "")
	if r.Empty() {
		t.Fatal("a roster with an enrolled operator is not empty")
	}
	// Revoking the last operator empties it again, which is what makes the
	// startup check meaningful: a node can be talked into a state where no gate
	// can be answered, and it should be able to notice.
	if _, err := r.Revoke("grace"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !r.Empty() {
		t.Error("a roster whose only operator is revoked should report empty")
	}
}

func TestRosterSurvivesReload(t *testing.T) {
	st := testStore(t)
	r, _ := Load(st)
	priv, pub := newKey(t)
	_ = r.Enroll("grace", pub, "Grace Hopper")

	fresh, err := Load(st)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	a := sign(t, "grace", priv, "node-a", "sess-1", "env-1", ledger.ApprovalApprove)
	if err := fresh.Check(a, "node-a", "sess-1", "env-1"); err != nil {
		t.Errorf("the roster did not survive a restart: %v", err)
	}
	if rows := fresh.List(); len(rows) != 1 || rows[0].Name != "Grace Hopper" {
		t.Errorf("listing after reload = %+v", rows)
	}
}

func TestCheckRejectsNil(t *testing.T) {
	r, _ := Load(testStore(t))
	if err := r.Check(nil, "n", "s", "e"); err != ledger.ErrNoApproval {
		t.Errorf("nil approval should report ErrNoApproval, got %v", err)
	}
}
