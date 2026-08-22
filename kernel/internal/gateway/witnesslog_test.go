package gateway

import (
	"testing"

	"aura/kernel/internal/ledger"
)

// The witness's log is only worth publishing if it is actually reachable, and
// only safe to publish if the follower's own baseline is not. Both halves are
// checked here, because getting the second one wrong (a prefix match instead of
// an enumeration) would let a stranger erase the record that convicts a witness.

func TestWitnessHeadIsServedAndSigned(t *testing.T) {
	g, h := namedGateway(t, "node-witnesslog")
	_ = g

	var head ledger.LogHead
	if code := getJSONFrom(t, h, "/v1/witness/head", &head); code != 200 {
		t.Fatalf("GET /v1/witness/head = %d", code)
	}
	if head.WitnessKey == "" || head.Signature == "" {
		t.Fatalf("head is not signed: %+v", head)
	}
	if err := ledger.VerifyLogHead(head); err != nil {
		t.Fatalf("the served head does not verify: %v", err)
	}
	// An empty witness still has a real, signable head. A special case here
	// would mean a monitor could not start following a witness until it had
	// already vouched for something.
	if head.Size != 0 {
		t.Errorf("a fresh witness reports %d entries", head.Size)
	}
}

func TestWitnessConsistencyDistinguishesAbsentFromZero(t *testing.T) {
	_, h := namedGateway(t, "node-witnesslog")

	// Absent is a caller error.
	var body map[string]any
	if code := getJSONFrom(t, h, "/v1/witness/consistency", &body); code != 400 {
		t.Errorf("a missing ?from= answered %d, want 400", code)
	}

	// Zero is a real question with a real answer: everything extends the empty
	// tree, so the proof is empty and the request succeeds. Conflating the two
	// broke exactly the case a monitor starts from.
	if code := getJSONFrom(t, h, "/v1/witness/consistency?from=0", &body); code != 200 {
		t.Fatalf("?from=0 answered %d, want 200", code)
	}
	if proof, ok := body["proof"].([]any); ok && len(proof) != 0 {
		t.Errorf("a proof from the empty tree should be empty, got %v", proof)
	}
}

func TestWitnessLogServesEntriesAndProofs(t *testing.T) {
	g, h := namedGateway(t, "node-witnesslog")

	var out struct {
		Entries []ledger.WitnessRecord `json:"entries"`
	}
	if code := getJSONFrom(t, h, "/v1/witness/log", &out); code != 200 {
		t.Fatalf("GET /v1/witness/log = %d", code)
	}
	if out.Entries == nil {
		t.Error("an empty log should serve an empty array, not null — a client " +
			"should not have to distinguish 'none' from 'malformed'")
	}

	// Nothing at position 1 yet, and the refusal must be a 404 rather than a
	// crash or an empty 200 that reads as "this exists and is blank".
	var body map[string]any
	if code := getJSONFrom(t, h, "/v1/witness/proof/1", &body); code != 404 {
		t.Errorf("a proof for an entry that does not exist answered %d, want 404", code)
	}
	_ = g
}

// The exemption list is enumerated, not prefix-matched. `/v1/witness/seen` is
// the follower's private audit baseline: public, it would let anyone lower the
// number an audit compares against, erasing from outside the evidence that
// would convict a witness of rewriting its log.
func TestOnlyTheWitnessOwnLogIsPublic(t *testing.T) {
	public := []string{
		"/v1/witness/head",
		"/v1/witness/log",
		"/v1/witness/consistency",
		"/v1/witness/proof/1",
	}
	for _, p := range public {
		if !openPath(p, true, registeredPathsForTest(t)) {
			t.Errorf("%s should be readable on an open witness", p)
		}
		if openPath(p, false, registeredPathsForTest(t)) {
			t.Errorf("%s is exempt even without --open-witness", p)
		}
	}

	private := []string{
		"/v1/witness/seen",
		"/v1/witness/seen?key=abc",
		"/v1/witness/seenx",
		"/v1/ledger",
		"/v1/skills",
	}
	for _, p := range private {
		if openPath(p, true, registeredPathsForTest(t)) {
			t.Errorf("%s is exposed on an open witness and must not be", p)
		}
	}
}

func TestRecordingASeenHeadRequiresItToVerify(t *testing.T) {
	_, h := namedGateway(t, "node-witnesslog")

	// A head nobody signed must not become an audit baseline: a baseline that
	// was never verified proves nothing and is indistinguishable, later, from
	// one that was.
	code, _ := do(t, h, "POST", "/v1/witness/seen", map[string]any{
		"witness_url": "http://elsewhere",
		"head": map[string]any{
			"witness_key": "AAAA", "size": 99,
			"root": "sha256:00", "signature": "AAAA",
		},
	})
	if code != 400 {
		t.Errorf("an unverifiable head was accepted with %d", code)
	}
}

// A monitor must never be talked into lowering its own baseline — that is the
// one move that would let a witness escape a contradiction it had already
// created.
func TestSeenBaselineOnlyMovesForward(t *testing.T) {
	g, h := namedGateway(t, "node-witnesslog")

	// The witness has to have vouched for something, or "lower" and "current"
	// are both zero and the test proves nothing.
	if _, err := g.Ldg.Seal(ledger.SealRequest{
		Session: "s1", Envelope: "e1", Actor: "acme/motor/x@1.0.0",
		Capability: "motor.api.writer", Decision: "allow", Outcome: "delivered",
		Policy: "sha256:p", Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("seal: %v", err)
	}
	stmt, err := g.Ldg.Statement(0)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	if _, err := g.Wit.Countersign(stmt); err != nil {
		t.Fatalf("countersign: %v", err)
	}

	first := g.Wit.LogHead()
	if first.Size == 0 {
		t.Fatal("setup: the witness log is still empty")
	}
	if code, _ := do(t, h, "POST", "/v1/witness/seen",
		map[string]any{"witness_url": "http://w", "head": first}); code != 200 {
		t.Fatalf("recording a valid head answered %d", code)
	}

	// Editing the size invalidates the signature, which is the outer defence:
	// a baseline can only be lowered by presenting a head the witness never
	// signed, and that is refused before the forward-only rule is even reached.
	lower := first
	lower.Size = 0
	if code, _ := do(t, h, "POST", "/v1/witness/seen",
		map[string]any{"witness_url": "http://w", "head": lower}); code == 200 {
		t.Error("a head that no longer verifies was accepted as a new baseline")
	}
}
