package broker

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// Rotation's quiet half. The signing change is the visible one; this is the
// one that destroys data when it is missing, because the secret-box key is
// derived from the node's private key and nothing warns you that the
// credentials are now unopenable until an effect needs one.

func newKeypair(t *testing.T) *signing.Keypair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return &signing.Keypair{Public: pub, Private: priv}
}

// succession builds a properly signed handover, the way `aura rotate` does.
func succession(t *testing.T, seq uint64, from, to *signing.Keypair) store.SuccessionRow {
	t.Helper()
	ts := time.Now().UnixMilli()
	fromPub, toPub := from.PublicB64(), to.PublicB64()
	payload := ledger.SuccessionPayload(seq, fromPub, toPub, ts)
	return store.SuccessionRow{
		Seq: seq, FromPubkey: fromPub, ToPubkey: toPub,
		FromSig: from.Sign(payload), ToSig: to.Sign(payload),
		Reason: "test", TS: ts,
	}
}

func TestSecretsSurviveRotationAndOpenUnderTheNewKey(t *testing.T) {
	b, st, outgoing := newRotatableBroker(t)
	for name, value := range map[string]string{
		"erp_token":   "sk-live-4417",
		"smtp.pass":   "hunter2",
		"webhook-key": "whsec_abc",
	} {
		if err := b.Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	incoming := newKeypair(t)
	rekeyed, err := b.Rekey(succession(t, 7, outgoing, incoming), incoming)
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if rekeyed != 3 {
		t.Errorf("re-encrypted %d secrets, expected 3", rekeyed)
	}

	// The broker that performed the rotation keeps working without a restart.
	if got, err := b.decrypt("erp_token"); err != nil || got != "sk-live-4417" {
		t.Errorf("in-process read after rotation: %q, %v", got, err)
	}

	// And so does a cold one opened with only the new key — which is what the
	// next start-up of the node actually is.
	cold, err := Open(st, incoming)
	if err != nil {
		t.Fatalf("open with the incoming key: %v", err)
	}
	for name, want := range map[string]string{
		"erp_token": "sk-live-4417", "smtp.pass": "hunter2", "webhook-key": "whsec_abc",
	} {
		got, err := cold.decrypt(name)
		if err != nil {
			t.Errorf("decrypt %s under the new key: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("secret %s came back %q, wanted %q", name, got, want)
		}
	}
}

func TestTheRetiredKeyNoLongerOpensTheSecrets(t *testing.T) {
	b, st, outgoing := newRotatableBroker(t)
	if err := b.Set("erp_token", "sk-live-4417"); err != nil {
		t.Fatalf("set: %v", err)
	}

	incoming := newKeypair(t)
	if _, err := b.Rekey(succession(t, 1, outgoing, incoming), incoming); err != nil {
		t.Fatalf("Rekey: %v", err)
	}

	stale, err := Open(st, outgoing)
	if err != nil {
		t.Fatalf("open with the retired key: %v", err)
	}
	if _, err := stale.decrypt("erp_token"); err == nil {
		t.Fatal("the retired key still opens the secrets — they were not re-encrypted")
	}
}

func TestAFailedReEncryptionLeavesTheNodeExactlyAsItWas(t *testing.T) {
	b, st, outgoing := newRotatableBroker(t)
	if err := b.Set("erp_token", "sk-live-4417"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// A secret this broker's key cannot open — the shape of "this data
	// directory and its identity/ do not belong together", discovered halfway
	// through a rotation.
	if err := st.SaveSecret("foreign", "bm90IG91cnM=", time.Now().UnixMilli()); err != nil {
		t.Fatalf("plant: %v", err)
	}

	incoming := newKeypair(t)
	if _, err := b.Rekey(succession(t, 1, outgoing, incoming), incoming); err == nil {
		t.Fatal("rotation reported success over a secret it could not decrypt")
	}

	// Nothing committed: no succession, and the readable secret still opens
	// under the key that was in force before the attempt.
	successions, err := st.LedgerSuccessions()
	if err != nil {
		t.Fatalf("read successions: %v", err)
	}
	if len(successions) != 0 {
		t.Errorf("a failed rotation recorded %d succession(s)", len(successions))
	}
	after, err := Open(st, outgoing)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, err := after.decrypt("erp_token"); err != nil || got != "sk-live-4417" {
		t.Errorf("the untouched secret did not survive the rollback: %q, %v", got, err)
	}
}

func TestRotatingANodeWithNoSecretsIsFine(t *testing.T) {
	b, st, outgoing := newRotatableBroker(t)
	incoming := newKeypair(t)
	rekeyed, err := b.Rekey(succession(t, 3, outgoing, incoming), incoming)
	if err != nil {
		t.Fatalf("Rekey on an empty broker: %v", err)
	}
	if rekeyed != 0 {
		t.Errorf("re-encrypted %d secrets on a node that has none", rekeyed)
	}
	if got, err := st.LedgerSuccessions(); err != nil || len(got) != 1 {
		t.Errorf("succession not recorded: %d, %v", len(got), err)
	}
}

func TestAKeyCannotHandOverTwice(t *testing.T) {
	b, st, outgoing := newRotatableBroker(t)

	first := newKeypair(t)
	if _, err := b.Rekey(succession(t, 1, outgoing, first), first); err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	// A second handover *out of the same key* is the fork an attacker holding
	// a stolen key would write. The unique index refuses it at the storage
	// layer, so it holds against anything that opens the file.
	second := newKeypair(t)
	if _, err := b.Rekey(succession(t, 2, outgoing, second), second); err == nil {
		t.Fatal("the same key handed over twice")
	}
	if got, _ := st.LedgerSuccessions(); len(got) != 1 {
		t.Errorf("%d successions recorded, expected 1", len(got))
	}
}

// newRotatableBroker is testBroker's sibling: same construction, but it hands
// back the keypair as well, because every test here is about what happens when
// that key is replaced.
func newRotatableBroker(t *testing.T) (*Broker, *store.Store, *signing.Keypair) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	keys, err := signing.LoadOrCreate(dir + "/identity")
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	b, err := Open(st, keys)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	return b, st, keys
}
