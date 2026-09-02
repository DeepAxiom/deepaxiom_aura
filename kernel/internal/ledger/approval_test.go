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
	a, err := SignApproval("grace", priv, "node-a", "sess-1", "env-1", ApprovalApprove, 1700000000000, nil)
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
	good, err := SignApproval("grace", priv, node, sess, env, ApprovalApprove, ts, nil)
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
		ApprovalApprove, time.Now().UnixMilli(), nil)
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
	good, _ := SignApproval("grace", priv, "n", "s", "e", ApprovalApprove, 1, nil)

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
	if _, err := SignApproval("", priv, "n", "s", "e", ApprovalApprove, 1, nil); err == nil {
		t.Error("signed an approval with no operator")
	}
	if _, err := SignApproval("g", priv, "n", "s", "", ApprovalApprove, 1, nil); err == nil {
		t.Error("signed an approval naming no delivery")
	}
	if _, err := SignApproval("g", priv, "n", "s", "e", "maybe", 1, nil); err == nil {
		t.Error("signed an approval with a decision outside the vocabulary")
	}
	if _, err := SignApproval("g", nil, "n", "s", "e", ApprovalApprove, 1, nil); err == nil {
		t.Error("signed an approval with no key")
	}
}

// Domain separation: an approval's signed bytes must not be reachable by any
// other signer in this codebase. Cheap to assert, and the failure it prevents
// (a checkpoint signature replayed as an approval) is unrecoverable.
func TestApprovalPayloadIsDomainSeparated(t *testing.T) {
	raw, err := ApprovalPayload("n", "s", "e", ApprovalApprove, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := string(raw)
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

// C4 v1.7 — what the approver was shown.
//
// The property under test is not "the field round-trips". It is that the field
// cannot be *edited*: an approval that bound a document must not still verify
// once someone swaps the document, drops the binding, or adds one that was
// never signed. A context that can be edited after the fact is decoration, and
// worse than none, because it reads as evidence.

func shownScreen(hex string) []ContextEntry {
	return []ContextEntry{{Label: "screen", Digest: "sha256:" + strings.Repeat(hex, 64)}}
}

func TestABareApprovalStillSignsExactlyTheV13Payload(t *testing.T) {
	// The frozen construction, written out rather than referenced, so a change
	// to ApprovalPayload that happens to be self-consistent still fails here.
	want := ApprovalDomain + ":node-a:sess-1:env-1:" + ApprovalApprove + ":1700000000000"
	raw, err := ApprovalPayload("node-a", "sess-1", "env-1", ApprovalApprove, 1700000000000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("a context-free approval no longer signs the v1.3 payload:\n got  %s\n want %s",
			raw, want)
	}
}

func TestAContextIsInsideTheSignature(t *testing.T) {
	priv, _ := testOperator(t)
	a, err := SignApproval("grace", priv, "node-a", "sess-1", "env-1",
		ApprovalApprove, 1700000000000, shownScreen("a"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := a.Verify("node-a", "sess-1"); err != nil {
		t.Fatalf("a freshly signed context-bound approval must verify: %v", err)
	}

	// Swapped: the same label, a different document.
	swapped := *a
	swapped.Context = shownScreen("b")
	if err := swapped.Verify("node-a", "sess-1"); err == nil {
		t.Error("an approval still verifies after the document it bound was swapped")
	}

	// Stripped: someone removes the binding and presents it as a plain answer.
	stripped := *a
	stripped.Context = nil
	if err := stripped.Verify("node-a", "sess-1"); err == nil {
		t.Error("dropping the context leaves a verifiable approval — the binding is optional in practice")
	}

	// Added: someone attaches a document to an approval that bound none.
	bare, err := SignApproval("grace", priv, "node-a", "sess-1", "env-1",
		ApprovalApprove, 1700000000000, nil)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	bare.Context = shownScreen("a")
	if err := bare.Verify("node-a", "sess-1"); err == nil {
		t.Error("a context can be attached after signing, which would let anyone claim consent")
	}
}

func TestTheOrderTheClientSentIsNotWhatWasSigned(t *testing.T) {
	priv, _ := testOperator(t)
	shown := []ContextEntry{
		{Label: "screen", Digest: "sha256:" + strings.Repeat("a", 64)},
		{Label: "certificate", Digest: "sha256:" + strings.Repeat("b", 64)},
	}
	a, err := SignApproval("grace", priv, "n", "s", "e", ApprovalApprove, 1, shown)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Handed back the other way round, as a client or a relay may well do.
	a.Context = []ContextEntry{shown[0], shown[1]}
	if err := a.Verify("n", "s"); err != nil {
		t.Fatalf("array order changed what was signed: %v", err)
	}
	// And what is left behind is canonical, so the entry hash is a function of
	// the set rather than of whatever order arrived.
	if a.Context[0].Label != "certificate" {
		t.Errorf("Verify left the context unsorted: %v", a.Context)
	}
}

func TestCanonicalContextCannotBeForgedByPunctuation(t *testing.T) {
	// The canonical form is "label=digest" joined by newlines, so the one way
	// two different contexts could collide is a label or digest that contains a
	// separator. Both charsets exclude them, and this is the assertion that
	// keeps them excluded.
	bad := [][]ContextEntry{
		{{Label: "a=b", Digest: "sha256:" + strings.Repeat("0", 64)}},
		{{Label: "a" + "\n" + "b", Digest: "sha256:" + strings.Repeat("0", 64)}},
		{{Label: "screen", Digest: "sha256:" + strings.Repeat("0", 64) + "\nx=y"}},
		{{Label: "screen", Digest: "sha256:" + strings.Repeat("0", 64) + "=z"}},
		{{Label: "Screen", Digest: "sha256:" + strings.Repeat("0", 64)}},
		{{Label: "9screen", Digest: "sha256:" + strings.Repeat("0", 64)}},
	}
	for _, entries := range bad {
		if _, err := CanonicalContext(entries); err == nil {
			t.Errorf("%q=%q was accepted, so two contexts can render to one string",
				entries[0].Label, entries[0].Digest)
		}
	}
}

func TestAContextCarriesDigestsAndNeverContent(t *testing.T) {
	// The ledger already refuses to store payloads. A context that accepted free
	// text would be a way around that rule, and the artifacts most worth binding
	// are exactly the ones most likely to carry personal data.
	for _, digest := range []string{
		"the note the doctor read",
		"sha256:NOTHEX",
		"sha256:" + strings.Repeat("a", 8),
		// 128 bits. Long enough to look like a hash, short enough that an
		// attacker can build two documents sharing it — and this field's whole
		// claim is that the artifact the signer held is the one now in hand.
		"md5:" + strings.Repeat("a", 32),
		"sha256:" + strings.Repeat("A", 64),
		"",
	} {
		if _, err := CanonicalContext([]ContextEntry{{Label: "screen", Digest: digest}}); err == nil {
			t.Errorf("%q was accepted as a digest", digest)
		}
	}
}

func TestOneLabelCannotMeanTwoThings(t *testing.T) {
	if _, err := CanonicalContext([]ContextEntry{
		{Label: "screen", Digest: "sha256:" + strings.Repeat("a", 64)},
		{Label: "screen", Digest: "sha256:" + strings.Repeat("b", 64)},
	}); err == nil {
		t.Error("a duplicated label was accepted, so an approval can claim one thing was two")
	}
	too := make([]ContextEntry, MaxContextEntries+1)
	for i := range too {
		too[i] = ContextEntry{
			Label:  "a" + strings.Repeat("b", i),
			Digest: "sha256:" + strings.Repeat("c", 64),
		}
	}
	if _, err := CanonicalContext(too); err == nil {
		t.Errorf("an approval may bind more than %d artifacts", MaxContextEntries)
	}
}

func TestUnboundNamesWhatIsMissing(t *testing.T) {
	a := &Approval{Context: shownScreen("a")}
	if got := a.Unbound([]string{"screen"}); len(got) != 0 {
		t.Errorf("Unbound reported %v for a label that is bound", got)
	}
	got := a.Unbound([]string{"screen", "certificate", "consent"})
	if len(got) != 2 || got[0] != "certificate" || got[1] != "consent" {
		t.Errorf("Unbound = %v, want [certificate consent]", got)
	}
	var none *Approval
	if got := none.Unbound([]string{"screen"}); len(got) != 1 {
		t.Error("a nil approval must count as binding nothing, not as binding everything")
	}
}
