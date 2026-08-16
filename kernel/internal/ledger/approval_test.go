package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// What these tests are actually defending.
//
// An approval is the only field in an entry the node cannot produce on its own,
// and that is the entire reason it exists: every other guarantee in C4 is the
// node signing a claim about itself, which is worth nothing against the case
// where the node is the thing under suspicion. So the tests that matter are the
// ones that try to move a signature somewhere it does not belong.

func testOperator(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return priv, base64.StdEncoding.EncodeToString(pub)
}

func TestApprovalRoundTrips(t *testing.T) {
	priv, pub := testOperator(t)
	a, err := SignApproval("grace", priv, "node-a", "sess-1", "env-1", ApprovalApprove, 1700000000000)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if a.Pubkey != pub {
		t.Errorf("the approval carries a different key than the one that signed it")
	}
	if err := a.Verify("node-a", "sess-1"); err != nil {
		t.Fatalf("a freshly signed approval must verify: %v", err)
	}
	if !a.Binds("env-1") {
		t.Error("the approval does not bind the delivery it was signed over")
	}
}

// Each of these is a transplant: a real signature, presented about something
// other than what it was made about. All four have to fail, and they fail for
// the same reason — the field is inside the signed bytes.
func TestApprovalIsNotTransplantable(t *testing.T) {
	priv, _ := testOperator(t)
	const (
		node = "node-a"
		sess = "sess-1"
		env  = "env-1"
		ts   = int64(1700000000000)
	)
	good, err := SignApproval("grace", priv, node, sess, env, ApprovalApprove, ts)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	t.Run("another node", func(t *testing.T) {
		if err := good.Verify("node-b", sess); err == nil {
			t.Error("an approval given on one node verified on another")
		}
	})

	t.Run("another session", func(t *testing.T) {
		if err := good.Verify(node, "sess-2"); err == nil {
			t.Error("an approval given in one session verified in another")
		}
	})

	t.Run("another delivery", func(t *testing.T) {
		moved := *good
		moved.Envelope = "env-2"
		if err := moved.Verify(node, sess); err == nil {
			t.Error("an approval for one delivery verified after being pointed at another")
		}
		if good.Binds("env-2") {
			t.Error("Binds accepted a delivery this approval does not answer")
		}
	})

	t.Run("flipped decision", func(t *testing.T) {
		// The attack this stops: intercept a refusal and forward it as an
		// approval by editing one JSON field.
		flipped := *good
		flipped.Decision = ApprovalDeny
		if err := flipped.Verify(node, sess); err == nil {
			t.Error("an approval survived having its decision rewritten")
		}
	})

	t.Run("backdated", func(t *testing.T) {
		moved := *good
		moved.TS = ts - 86400000
		if err := moved.Verify(node, sess); err == nil {
			t.Error("an approval survived being backdated a day")
		}
	})
}

// The impersonation case. Anyone can mint a keypair and claim to be Grace; what
// they cannot do is produce a signature that verifies against the key the node
// enrolled for her. Verify alone cannot catch this — it checks authorship, not
// enrollment — so this test pins the half it *does* catch and approver.Check
// covers the other.
func TestApprovalBySomeoneElsesKeyStillVerifiesAsItsOwnAuthor(t *testing.T) {
	attacker, attackerPub := testOperator(t)
	_, realPub := testOperator(t)

	forged, err := SignApproval("grace", attacker, "node-a", "sess-1", "env-1",
		ApprovalApprove, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// It verifies — the attacker really did sign it — which is exactly why the
	// roster check is not optional. The signature proves *who signed*, and the
	// name in the entry is a claim until the roster confirms it.
	if err := forged.Verify("node-a", "sess-1"); err != nil {
		t.Fatalf("a self-consistent forgery should still be internally valid: %v", err)
	}
	if forged.Pubkey == realPub {
		t.Fatal("test setup: the two keypairs collided")
	}
	if forged.Pubkey != attackerPub {
		t.Error("the forged approval does not carry the key that made it")
	}
}

func TestApprovalRejectsMalformedInput(t *testing.T) {
	priv, _ := testOperator(t)
	good, _ := SignApproval("grace", priv, "n", "s", "e", ApprovalApprove, 1)

	cases := map[string]func(*Approval){
		"no operator":  func(a *Approval) { a.Operator = "" },
		"no envelope":  func(a *Approval) { a.Envelope = "" },
		"bad decision": func(a *Approval) { a.Decision = "maybe" },
		"bad pubkey":   func(a *Approval) { a.Pubkey = "not base64!!" },
		"short pubkey": func(a *Approval) { a.Pubkey = base64.StdEncoding.EncodeToString([]byte("short")) },
		"bad sig":      func(a *Approval) { a.Sig = "not base64!!" },
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			a := *good
			mangle(&a)
			if err := a.Verify("n", "s"); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}

	var nilA *Approval
	if err := nilA.Verify("n", "s"); err != ErrNoApproval {
		t.Errorf("a nil approval should report ErrNoApproval, got %v", err)
	}
}

// Signing refuses to make a statement that could not be verified later, rather
// than producing one that fails mysteriously at the gate.
func TestSignApprovalRefusesIncompleteStatements(t *testing.T) {
	priv, _ := testOperator(t)
	if _, err := SignApproval("", priv, "n", "s", "e", ApprovalApprove, 1); err == nil {
		t.Error("signed an approval with no operator")
	}
	if _, err := SignApproval("g", priv, "n", "s", "", ApprovalApprove, 1); err == nil {
		t.Error("signed an approval naming no delivery")
	}
	if _, err := SignApproval("g", priv, "n", "s", "e", "maybe", 1); err == nil {
		t.Error("signed an approval with a decision outside the vocabulary")
	}
	if _, err := SignApproval("g", nil, "n", "s", "e", ApprovalApprove, 1); err == nil {
		t.Error("signed an approval with no key")
	}
}

// Domain separation: an approval's signed bytes must not be reachable by any
// other signer in this codebase. Cheap to assert, and the failure it prevents
// (a checkpoint signature replayed as an approval) is unrecoverable.
func TestApprovalPayloadIsDomainSeparated(t *testing.T) {
	p := string(ApprovalPayload("n", "s", "e", ApprovalApprove, 1))
	if !strings.HasPrefix(p, ApprovalDomain+":") {
		t.Fatalf("approval payload %q is not domain-separated", p)
	}
	if strings.HasPrefix(p, "aura-ledger-checkpoint") {
		t.Fatal("approval and checkpoint payloads share a prefix")
	}
	// Every component has to appear, or something is not actually bound.
	for _, want := range []string{"n", "s", "e", ApprovalApprove, "1"} {
		if !strings.Contains(p, want) {
			t.Errorf("approval payload does not cover %q: %s", want, p)
		}
	}
}
