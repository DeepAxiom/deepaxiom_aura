package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// The timeline is what decides which key each checkpoint is checked against,
// so everything that could make it describe the wrong history has to be a
// refusal rather than a best effort. Each test below is one such thing.

func key(t *testing.T) *signing.Keypair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return &signing.Keypair{Public: pub, Private: priv}
}

func handover(seq uint64, from, to *signing.Keypair) store.SuccessionRow {
	const ts = 1_700_000_000_000
	fromPub, toPub := from.PublicB64(), to.PublicB64()
	payload := SuccessionPayload(seq, fromPub, toPub, ts)
	return store.SuccessionRow{
		Seq: seq, FromPubkey: fromPub, ToPubkey: toPub,
		FromSig: from.Sign(payload), ToSig: to.Sign(payload),
		Reason: "test", TS: ts,
	}
}

func TestANodeThatNeverRotatedHasOneSegmentCoveringEverything(t *testing.T) {
	k := key(t)
	timeline, err := BuildKeyTimeline(nil, k.PublicB64())
	if err != nil {
		t.Fatalf("BuildKeyTimeline: %v", err)
	}
	if timeline.Rotations != 0 || len(timeline.Segments) != 1 {
		t.Fatalf("got %d segments / %d rotations, wanted 1 / 0",
			len(timeline.Segments), timeline.Rotations)
	}
	for _, seq := range []uint64{1, 500, 1 << 40} {
		if got := timeline.KeyAt(seq); got != k.PublicB64() {
			t.Errorf("KeyAt(%d) did not return the only key there is", seq)
		}
	}
}

func TestTheBoundarySequenceBelongsToTheRetiredKey(t *testing.T) {
	oldKey, newKey := key(t), key(t)
	rows := []store.SuccessionRow{handover(10, oldKey, newKey)}

	timeline, err := BuildKeyTimeline(rows, newKey.PublicB64())
	if err != nil {
		t.Fatalf("BuildKeyTimeline: %v", err)
	}
	if timeline.Rotations != 1 {
		t.Fatalf("got %d rotations, wanted 1", timeline.Rotations)
	}
	// Seq is the last entry sealed under the outgoing key, so 10 is still the
	// old key's and 11 is the new one's. Off by one here would silently fail
	// every checkpoint at the boundary.
	if got := timeline.KeyAt(10); got != oldKey.PublicB64() {
		t.Error("seq 10 (the boundary itself) was not attributed to the retired key")
	}
	if got := timeline.KeyAt(11); got != newKey.PublicB64() {
		t.Error("seq 11 was not attributed to the incoming key")
	}
}

func TestThreeRotationsReconstructInOrderFromTheCurrentKeyAlone(t *testing.T) {
	k0, k1, k2, k3 := key(t), key(t), key(t), key(t)
	rows := []store.SuccessionRow{
		handover(10, k0, k1),
		handover(25, k1, k2),
		handover(60, k2, k3),
	}
	// The verifier is handed only k3 — the key sitting in identity/ed25519.pub.
	timeline, err := BuildKeyTimeline(rows, k3.PublicB64())
	if err != nil {
		t.Fatalf("BuildKeyTimeline: %v", err)
	}
	if timeline.Rotations != 3 {
		t.Fatalf("got %d rotations, wanted 3", timeline.Rotations)
	}
	for _, tc := range []struct {
		seq  uint64
		want *signing.Keypair
		why  string
	}{
		{1, k0, "the original key"}, {10, k0, "its last sealed entry"},
		{11, k1, "the first after the first handover"}, {25, k1, "its last"},
		{26, k2, "the third key's first"}, {60, k2, "its last"},
		{61, k3, "current"}, {9_999, k3, "still current"},
	} {
		if got := timeline.KeyAt(tc.seq); got != tc.want.PublicB64() {
			t.Errorf("KeyAt(%d) picked the wrong key — expected %s", tc.seq, tc.why)
		}
	}
}

func TestAHandoverTheOutgoingKeyDidNotAuthorizeIsRefused(t *testing.T) {
	victim, attacker, unrelated := key(t), key(t), key(t)

	// The attacker wants the chain to end at a key they hold. They can sign as
	// themselves; they cannot sign as the victim, so they borrow a signature.
	forged := handover(10, victim, attacker)
	stolen := handover(10, unrelated, attacker)
	forged.FromSig = stolen.FromSig

	if _, err := BuildKeyTimeline([]store.SuccessionRow{forged}, attacker.PublicB64()); err == nil {
		t.Fatal("accepted a handover the outgoing key never authorized")
	} else if !strings.Contains(err.Error(), "outgoing key") {
		t.Errorf("error does not say which signature failed: %v", err)
	}
}

func TestAHandoverToAKeyNobodyProvedTheyHoldIsRefused(t *testing.T) {
	oldKey, target, other := key(t), key(t), key(t)

	// Naming someone else's published key as your successor: the ledger's
	// future would belong to a key this node cannot sign with.
	rec := handover(10, oldKey, target)
	rec.ToSig = handover(10, oldKey, other).ToSig

	if _, err := BuildKeyTimeline([]store.SuccessionRow{rec}, target.PublicB64()); err == nil {
		t.Fatal("accepted a handover to a key that never proved possession")
	} else if !strings.Contains(err.Error(), "incoming key") {
		t.Errorf("error does not say which signature failed: %v", err)
	}
}

func TestTwoHandoversToTheSameKeyForkTheHistoryAndAreRefused(t *testing.T) {
	a, b, target := key(t), key(t), key(t)
	rows := []store.SuccessionRow{handover(10, a, target), handover(20, b, target)}

	if _, err := BuildKeyTimeline(rows, target.PublicB64()); err == nil {
		t.Fatal("accepted two chains of custody ending at the same key")
	} else if !strings.Contains(err.Error(), "fork") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

func TestRecordsUnreachableFromTheCurrentKeyAreRefused(t *testing.T) {
	k0, k1 := key(t), key(t)
	stranger, strangerNext := key(t), key(t)
	rows := []store.SuccessionRow{
		handover(10, k0, k1),
		handover(30, stranger, strangerNext), // belongs to no chain from k1
	}

	if _, err := BuildKeyTimeline(rows, k1.PublicB64()); err == nil {
		t.Fatal("accepted succession rows that no chain from the current key reaches")
	} else if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

func TestAKeyHandingOverToItselfIsRefused(t *testing.T) {
	k := key(t)
	if err := VerifySuccession(handover(10, k, k)); err == nil {
		t.Fatal("accepted a key succeeding itself")
	}
}

func TestSequencesThatDoNotIncreaseAreRefused(t *testing.T) {
	k0, k1, k2 := key(t), key(t), key(t)
	// The second handover claims to end *before* the first one did.
	rows := []store.SuccessionRow{handover(40, k0, k1), handover(15, k1, k2)}

	if _, err := BuildKeyTimeline(rows, k2.PublicB64()); err == nil {
		t.Fatal("accepted succession sequences that run backwards")
	} else if !strings.Contains(err.Error(), "out of order") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

func TestAKeyFromADifferentLedgerAnchorsNothing(t *testing.T) {
	k0, k1, foreign := key(t), key(t), key(t)
	rows := []store.SuccessionRow{handover(10, k0, k1)}

	// Verifying this ledger with someone else's public key must fail loudly
	// rather than quietly reporting a one-segment history that happens to
	// verify no checkpoints.
	if _, err := BuildKeyTimeline(rows, foreign.PublicB64()); err == nil {
		t.Fatal("a key belonging to another node built a timeline over this ledger")
	}
}

func TestNoAnchorKeyIsAnError(t *testing.T) {
	if _, err := BuildKeyTimeline(nil, ""); err == nil {
		t.Fatal("built a timeline with nothing to anchor it to")
	}
}
