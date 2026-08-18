package broker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// The claim under test is a strong one and deserves to be attacked directly:
// *going around the Effect Checkpoint leaves you without the credential*. Each
// test below is one way of trying to get a secret without a genuine, current,
// matching receipt, and every one of them has to come back empty.

func testBroker(t *testing.T) (*Broker, *store.Store, *ledger.Ledger) {
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
	ldg, err := ledger.Open(st, "node-test", keys)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	b, err := Open(st, keys)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	return b, st, ldg
}

// sealOne puts a real effect in the chain and returns its receipt, so every
// test below is exercising the actual ledger rather than a stand-in.
func sealOne(t *testing.T, ldg *ledger.Ledger, capability, outcome string) string {
	t.Helper()
	receipt, err := ldg.Seal(ledger.SealRequest{
		Session: "sess-1", Envelope: "env-1", Cause: "cause-1",
		Actor: "acme/motor/erp@1.0.0", Capability: capability,
		Decision: spec.DecisionGate, Outcome: outcome,
		Policy: "sha256:policy", Payload: []byte(`{"amount":1}`),
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return receipt
}

func TestSecretRoundTripsThroughAValidReceipt(t *testing.T) {
	b, _, ldg := testBroker(t)
	if err := b.Set("erp_token", "hunter2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	receipt := sealOne(t, ldg, "motor.api.erp.create_invoice", spec.OutcomeDelivered)

	grant, err := b.Resolve(receipt, "motor.api.erp.create_invoice", []string{"erp_token"})
	if err != nil {
		t.Fatalf("a genuine receipt was refused: %v", err)
	}
	if grant.Values["erp_token"] != "hunter2" {
		t.Errorf("got %q, want the stored value", grant.Values["erp_token"])
	}
}

// The headline case: no receipt, no credential. This is what "bypassing the
// gate gets you a 401" means in code.
func TestNoReceiptGetsNothing(t *testing.T) {
	b, _, _ := testBroker(t)
	_ = b.Set("erp_token", "hunter2")
	if _, err := b.Resolve("", "motor.api.erp.create_invoice", []string{"erp_token"}); err == nil {
		t.Fatal("a secret was released with no receipt at all")
	}
}

func TestForgedReceiptGetsNothing(t *testing.T) {
	b, _, _ := testBroker(t)
	_ = b.Set("erp_token", "hunter2")
	_, err := b.Resolve("sha256:"+strings.Repeat("ab", 32),
		"motor.api.erp.create_invoice", []string{"erp_token"})
	if err == nil {
		t.Fatal("an invented receipt bought a credential")
	}
	if !strings.Contains(err.Error(), "no effect this node sealed") {
		t.Errorf("unexpected refusal: %v", err)
	}
}

// The lateral-movement case. A skill that legitimately produces one effect must
// not be able to spend that receipt on a different capability's credential —
// otherwise any skill that ever wrote anything could reach the payments token.
func TestAReceiptCannotBeSpentOnAnotherCapability(t *testing.T) {
	b, _, ldg := testBroker(t)
	_ = b.Set("payments_key", "very-secret")
	receipt := sealOne(t, ldg, "motor.api.crm.add_note", spec.OutcomeDelivered)

	_, err := b.Resolve(receipt, "motor.payments.refund", []string{"payments_key"})
	if err == nil {
		t.Fatal("a receipt for one capability bought another capability's credential")
	}
	if !strings.Contains(err.Error(), "and no other") {
		t.Errorf("unexpected refusal: %v", err)
	}
}

// A refused effect is evidence that something was *not* authorized. It must buy
// less than nothing.
func TestADeniedEffectBuysNothing(t *testing.T) {
	b, _, ldg := testBroker(t)
	_ = b.Set("erp_token", "hunter2")
	receipt := sealOne(t, ldg, "motor.api.erp.create_invoice", spec.OutcomeDenied)

	if _, err := b.Resolve(receipt, "motor.api.erp.create_invoice", []string{"erp_token"}); err == nil {
		t.Fatal("an effect a human refused still released the credential")
	}
}

// A receipt is not a bearer token with a shelf life. Redeeming one repeatedly
// would turn a single approved effect into a standing licence, which is the
// exact property this design removes.
func TestAReceiptIsNotReusableForever(t *testing.T) {
	b, _, ldg := testBroker(t)
	_ = b.Set("erp_token", "hunter2")
	receipt := sealOne(t, ldg, "motor.api.erp.create_invoice", spec.OutcomeDelivered)

	for i := 0; i < maxUses; i++ {
		if _, err := b.Resolve(receipt, "motor.api.erp.create_invoice", []string{"erp_token"}); err != nil {
			t.Fatalf("redemption %d was refused early: %v", i+1, err)
		}
	}
	if _, err := b.Resolve(receipt, "motor.api.erp.create_invoice", []string{"erp_token"}); err == nil {
		t.Fatalf("a receipt was redeemable more than %d times", maxUses)
	}
}

func TestAnExpiredReceiptBuysNothing(t *testing.T) {
	b, st, _ := testBroker(t)
	_ = b.Set("erp_token", "hunter2")

	// Seal directly through the store with an old timestamp: the age check has
	// to work against what is *stored*, not against when this process happened
	// to see it.
	old := ledger.Entry{
		Seq: 1, TS: time.Now().Add(-2 * ReceiptTTL).UnixMilli(), Node: "node-test",
		Session: "sess-1", Envelope: "env-1", Actor: "acme/motor/erp@1.0.0",
		Capability: "motor.api.erp.create_invoice",
		Decision:   spec.DecisionAllow, Outcome: spec.OutcomeDelivered,
		Policy: "sha256:policy", PayloadSHA256: "abc",
	}
	raw, _ := json.Marshal(old)
	if err := st.AppendLedgerEntry(1, old.Hash(), old.Session, raw); err != nil {
		t.Fatalf("append: %v", err)
	}

	_, err := b.Resolve(old.Hash(), "motor.api.erp.create_invoice", []string{"erp_token"})
	if err == nil {
		t.Fatal("a receipt older than the TTL still bought a credential")
	}
	if !strings.Contains(err.Error(), "old") {
		t.Errorf("unexpected refusal: %v", err)
	}
}

func TestSecretsAreEncryptedAtRestAndNeverListedByValue(t *testing.T) {
	b, st, _ := testBroker(t)
	if err := b.Set("erp_token", "hunter2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	ct, ok, err := st.Secret("erp_token")
	if err != nil || !ok {
		t.Fatalf("stored secret unreadable: ok=%v err=%v", ok, err)
	}
	if strings.Contains(ct, "hunter2") {
		t.Error("the plaintext is sitting in the database")
	}
	names, err := b.Names()
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	if len(names) != 1 || names[0] != "erp_token" {
		t.Errorf("names = %v", names)
	}
}

// The ciphertext is bound to its own name, so swapping two rows in the database
// — a read-only token into the row the write path reads — does not silently
// change what a credential is.
func TestCiphertextCannotBeMovedBetweenNames(t *testing.T) {
	b, st, _ := testBroker(t)
	_ = b.Set("read_token", "read-only")
	_ = b.Set("write_token", "dangerous")

	ct, _, _ := st.Secret("read_token")
	if err := st.SaveSecret("write_token", ct, time.Now().UnixMilli()); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := b.decrypt("write_token"); err == nil {
		t.Fatal("a ciphertext moved to another secret's row still decrypted")
	}
}

func TestResolveInSubstitutesOnlyWithAReceipt(t *testing.T) {
	b, _, ldg := testBroker(t)
	_ = b.Set("erp_token", "hunter2")
	headers := map[string]string{
		"Authorization": "Bearer ${secret:erp_token}",
		"X-Trace":       "static",
	}

	if _, err := b.ResolveIn("", "motor.api.erp.create_invoice", headers); err == nil {
		t.Fatal("headers referencing a secret resolved with no receipt")
	}

	receipt := sealOne(t, ldg, "motor.api.erp.create_invoice", spec.OutcomeDelivered)
	out, err := b.ResolveIn(receipt, "motor.api.erp.create_invoice", headers)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out["Authorization"] != "Bearer hunter2" {
		t.Errorf("Authorization = %q", out["Authorization"])
	}
	if out["X-Trace"] != "static" {
		t.Errorf("a header with no reference was altered: %q", out["X-Trace"])
	}
}

// Headers with no reference need no receipt: this stays entirely opt-in for a
// projection whose target authenticates with nothing.
func TestHeadersWithoutSecretsNeedNoReceipt(t *testing.T) {
	b, _, _ := testBroker(t)
	in := map[string]string{"X-Trace": "static"}
	out, err := b.ResolveIn("", "motor.api.erp.create_invoice", in)
	if err != nil {
		t.Fatalf("plain headers required a receipt: %v", err)
	}
	if out["X-Trace"] != "static" {
		t.Errorf("out = %v", out)
	}
	if NeedsSecret(in) {
		t.Error("NeedsSecret said yes for headers with no reference")
	}
	if !NeedsSecret(map[string]string{"A": "${secret:x}"}) {
		t.Error("NeedsSecret missed a reference")
	}
}

func TestSetValidatesTheName(t *testing.T) {
	b, _, _ := testBroker(t)
	if err := b.Set("", "v"); err == nil {
		t.Error("stored a secret with no name")
	}
	if err := b.Set("has spaces", "v"); err == nil {
		t.Error("stored a name that cannot be referenced as ${secret:…}")
	}
}

func TestMissingSecretIsNamedNotGuessed(t *testing.T) {
	b, _, ldg := testBroker(t)
	receipt := sealOne(t, ldg, "motor.api.erp.create_invoice", spec.OutcomeDelivered)
	_, err := b.Resolve(receipt, "motor.api.erp.create_invoice", []string{"absent"})
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Errorf("a missing secret should be named in the error, got: %v", err)
	}
}

// A graph is a JSON document anyone able to reach /v1/graphs can register, so
// `"gate": "none"` on a motor edge is an authorization written by whoever wrote
// the graph. Under the built-in default policy that waiver is honoured — which
// is right, a laptop node should be able to run its own seeded voice graph — and
// it seals a perfectly valid `delivered` entry.
//
// That entry must not buy a credential. Otherwise the broker's whole argument
// inverts: instead of "skipping the gate gets you a 401", registering a graph
// that skips the gate would get you the secret, and the strength claimed to come
// from the ledger would come from nothing.
func TestAWaivedEffectBuysNothing(t *testing.T) {
	b, _, ldg := testBroker(t)
	if err := b.Set("erp_token", "s3cr3t"); err != nil {
		t.Fatalf("set: %v", err)
	}

	receipt, err := ldg.Seal(ledger.SealRequest{
		Session: "sess-waived", Envelope: "env-1", Cause: "cause-1",
		Actor: "acme/motor/erp@1.0.0", Capability: "motor.erp.write",
		// Everything a genuine, spendable receipt has...
		Decision: spec.DecisionAllow, Outcome: spec.OutcomeDelivered,
		Policy: "sha256:policy", Payload: []byte(`{"amount":1}`),
		// ...except that the graph, not the node, is why there was no gate.
		Waived: true,
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	grant, err := b.Resolve(receipt, "motor.erp.write", []string{"erp_token"})
	if err == nil {
		t.Fatalf("a graph waived its own gate and was handed %v", grant.Values)
	}
	if !strings.Contains(err.Error(), "graph") {
		t.Errorf("the refusal should say a graph waiver is what was refused; got: %v", err)
	}
}

// The other half of the same rule: an effect the *policy* allowed is spendable.
// If this failed, the fix above would have closed the hole by breaking the
// feature, which is not closing it.
func TestAPolicyAllowedEffectStillBuysTheSecret(t *testing.T) {
	b, _, ldg := testBroker(t)
	if err := b.Set("erp_token", "s3cr3t"); err != nil {
		t.Fatalf("set: %v", err)
	}
	receipt, err := ldg.Seal(ledger.SealRequest{
		Session: "sess-allowed", Envelope: "env-1", Cause: "cause-1",
		Actor: "acme/motor/erp@1.0.0", Capability: "motor.erp.write",
		Decision: spec.DecisionAllow, Outcome: spec.OutcomeDelivered,
		Policy: "sha256:policy", Payload: []byte(`{"amount":1}`),
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	grant, err := b.Resolve(receipt, "motor.erp.write", []string{"erp_token"})
	if err != nil {
		t.Fatalf("a policy-allowed effect was refused its credential: %v", err)
	}
	if grant.Values["erp_token"] != "s3cr3t" {
		t.Errorf("got %q, want the stored secret", grant.Values["erp_token"])
	}
}
