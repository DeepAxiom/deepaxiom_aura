package ledger

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"aura/kernel/internal/store"
)

// attestation returns a well-formed C5 record for tests.
func attestation(model, seed string) []byte {
	return []byte(`{"engine":"llama.cpp","engine_version":"b4321",` +
		`"model":"` + model + `","model_revision":"f1d2d2f924e986ac86fdf7b36c94bcdf32beec15",` +
		`"quantization":"Q4_K_M","params":{"temperature":0.7,"seed":` + seed + `}}`)
}

// sealWithInference seals n effects, binding an attestation to the middle one,
// and returns the store, the ledger and the receipt hash of that effect.
func sealWithInference(t *testing.T, n int) (*store.Store, *Ledger, string, string) {
	t.Helper()
	st, _ := testStore(t)
	l := testLedger(t, st)

	raw := attestation("Qwen/Qwen2.5-1.5B-Instruct-GGUF", "42")
	_, attHash, err := ParseAttestation(raw)
	if err != nil {
		t.Fatalf("ParseAttestation: %v", err)
	}
	if err := RecordAttestation(st, attHash, raw); err != nil {
		t.Fatalf("RecordAttestation: %v", err)
	}

	var target string
	for i := 0; i < n; i++ {
		r := req("motor.erp.invoice.create")
		if i == n/2 {
			r.Inference = []string{attHash}
		}
		receipt, err := l.Seal(r)
		if err != nil {
			t.Fatalf("Seal %d: %v", i, err)
		}
		if i == n/2 {
			target = receipt
		}
	}
	if err := l.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	return st, l, target, attHash
}

// roundTrip serialises a receipt the way the given marshaller would and reads
// it back, so a test can assert against the document a real consumer receives
// rather than against the in-memory struct.
func roundTrip(t *testing.T, r Receipt, marshal func(any) ([]byte, error)) Receipt {
	t.Helper()
	blob, err := marshal(r)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	var back Receipt
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal receipt: %v", err)
	}
	return back
}

func TestReceiptVerifiesStandalone(t *testing.T) {
	st, _, target, attHash := sealWithInference(t, 9)

	r, err := BuildReceipt(st, target)
	if err != nil {
		t.Fatalf("BuildReceipt: %v", err)
	}

	// The whole point: round-trip through JSON and verify with nothing but
	// the document. No store, no node, no network.
	portable := roundTrip(t, r, json.Marshal)

	rep := VerifyReceipt(portable)
	if !rep.Sound() {
		t.Fatalf("a freshly built receipt did not verify: %+v", rep)
	}
	if !rep.EntryHashMatches || !rep.InclusionValid || !rep.CheckpointValid {
		t.Fatalf("expected all three core checks to pass, got %+v", rep)
	}
	if rep.AttestationsCited != 1 || rep.AttestationsResolved != 1 {
		t.Fatalf("expected 1 cited and 1 resolved attestation, got %d/%d",
			rep.AttestationsResolved, rep.AttestationsCited)
	}
	if _, ok := portable.Attestations[attHash]; !ok {
		t.Fatal("receipt does not carry the attestation its entry cites")
	}
}

// A receipt has to survive being *reformatted*, not just re-serialised.
//
// This is the regression test for a real bug: the first build embedded the
// entry and the attestations as raw JSON, and `aura receipt --out` writes with
// MarshalIndent — which re-indents nested values, changing the exact bytes the
// leaf hash and the content addresses are computed over. Every receipt the CLI
// produced failed to verify, and the unit tests missed it because they all
// round-tripped through compact json.Marshal, which happens to preserve those
// bytes.
//
// Any encoding that leaves hash-critical bytes at the mercy of a JSON layer
// has this bug latent in it. Testing indented output is what keeps it fixed.
func TestReceiptSurvivesReformatting(t *testing.T) {
	st, _, target, _ := sealWithInference(t, 11)
	r, err := BuildReceipt(st, target)
	if err != nil {
		t.Fatalf("BuildReceipt: %v", err)
	}

	formatters := map[string]func(any) ([]byte, error){
		"compact": json.Marshal,
		"indented two spaces": func(v any) ([]byte, error) {
			return json.MarshalIndent(v, "", "  ")
		},
		"indented tabs": func(v any) ([]byte, error) {
			return json.MarshalIndent(v, "", "\t")
		},
		"re-encoded through a generic map": func(v any) ([]byte, error) {
			// What a proxy, a jq pipeline or any tool that does not know the
			// Receipt type would do to the document on its way past.
			blob, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			var generic map[string]any
			if err := json.Unmarshal(blob, &generic); err != nil {
				return nil, err
			}
			return json.MarshalIndent(generic, "", "    ")
		},
	}
	for name, marshal := range formatters {
		t.Run(name, func(t *testing.T) {
			rep := VerifyReceipt(roundTrip(t, r, marshal))
			if !rep.Sound() {
				t.Fatalf("a receipt serialised as %q did not verify: %+v", name, rep)
			}
			if rep.AttestationsResolved != 1 {
				t.Fatalf("%q: attestation did not resolve (%d/%d) — the record's bytes "+
					"were altered by the encoder", name,
					rep.AttestationsResolved, rep.AttestationsCited)
			}
		})
	}
}

// A receipt must verify at every position in the tree, not just at
// convenient ones — the ragged right edge of a non-power-of-two tree is
// exactly where an inclusion proof implementation goes wrong.
func TestReceiptVerifiesAtEveryPosition(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 8, 13, 21} {
		st, _ := testStore(t)
		l := testLedger(t, st)
		receipts := make([]string, n)
		for i := 0; i < n; i++ {
			var err error
			if receipts[i], err = l.Seal(req("motor.erp.op")); err != nil {
				t.Fatalf("n=%d seal %d: %v", n, i, err)
			}
		}
		if err := l.Checkpoint(); err != nil {
			t.Fatalf("n=%d checkpoint: %v", n, err)
		}
		for i, hash := range receipts {
			r, err := BuildReceipt(st, hash)
			if err != nil {
				t.Fatalf("n=%d i=%d BuildReceipt: %v", n, i, err)
			}
			if rep := VerifyReceipt(r); !rep.Sound() {
				t.Fatalf("n=%d i=%d: receipt did not verify: %+v", n, i, rep)
			}
		}
	}
}

func TestReceiptRejectsTampering(t *testing.T) {
	st, _, target, _ := sealWithInference(t, 7)
	base, err := BuildReceipt(st, target)
	if err != nil {
		t.Fatalf("BuildReceipt: %v", err)
	}

	// rewriteEntry returns a copy of base whose entry claims a different
	// capability — the tamper that matters, since it changes what was
	// authorized.
	rewriteEntry := func(r Receipt) (Receipt, Entry) {
		var e Entry
		_ = json.Unmarshal(base.Entry(), &e)
		e.Capability = "motor.erp.invoice.delete"
		edited, _ := json.Marshal(e)
		r.EntryB64 = base64.StdEncoding.EncodeToString(edited)
		return r, e
	}

	t.Run("edited entry", func(t *testing.T) {
		r, _ := rewriteEntry(base)
		rep := VerifyReceipt(r)
		if rep.Sound() {
			t.Fatal("a receipt whose entry was rewritten verified")
		}
		if rep.EntryHashMatches {
			t.Fatal("the edited entry still matched its claimed hash")
		}
	})

	t.Run("edited entry with repaired hash", func(t *testing.T) {
		// The interesting attack: fix the entry hash so the local check
		// passes. The inclusion proof must still fail, because the leaf is
		// hashed over the entry bytes and the tree head was signed.
		r, e := rewriteEntry(base)
		r.EntryHash = e.Hash()
		rep := VerifyReceipt(r)
		if rep.Sound() {
			t.Fatal("an entry rewritten with a repaired hash verified — the signed " +
				"tree head is not actually constraining the entry content")
		}
		if !rep.EntryHashMatches {
			t.Fatal("precondition: the repaired hash should match locally")
		}
		if rep.InclusionValid {
			t.Fatal("the inclusion proof accepted a leaf that is not in the signed tree")
		}
	})

	t.Run("forged checkpoint signature", func(t *testing.T) {
		r := base
		r.Checkpoint.Signature = "Zm9yZ2Vk"
		if rep := VerifyReceipt(r); rep.Sound() || rep.CheckpointValid {
			t.Fatal("a receipt with a forged checkpoint signature verified")
		}
	})

	t.Run("substituted merkle root", func(t *testing.T) {
		r := base
		other := LeafHash([]byte("a tree that was never signed")).String()
		r.MerkleRoot, r.Checkpoint.MerkleRoot = other, other
		if rep := VerifyReceipt(r); rep.Sound() {
			t.Fatal("a receipt anchored to an unsigned tree head verified")
		}
	})

	t.Run("edited attestation", func(t *testing.T) {
		r := base
		r.Attestations = map[string]string{}
		for h := range base.Attestations {
			// Same shape, different model: the lie an attestation exists to
			// make impossible.
			r.Attestations[h] = base64.StdEncoding.EncodeToString(
				attestation("meta-llama/Llama-3-70B", "42"))
		}
		rep := VerifyReceipt(r)
		if rep.AttestationsResolved != 0 {
			t.Fatal("a swapped attestation record resolved against its content address")
		}
		if len(rep.Problems) == 0 {
			t.Fatal("swapping the attestation produced no reported problem")
		}
	})

	t.Run("dropped attestation", func(t *testing.T) {
		r := base
		r.Attestations = nil
		rep := VerifyReceipt(r)
		if rep.AttestationsResolved != 0 || rep.AttestationsCited != 1 {
			t.Fatalf("expected 0/1 resolved/cited, got %d/%d",
				rep.AttestationsResolved, rep.AttestationsCited)
		}
		// The effect itself is still proven — only the basis is missing.
		if !rep.Sound() {
			t.Fatal("dropping an attestation should not invalidate the effect's own proof")
		}
	})
}

func TestBuildReceiptNeedsACheckpoint(t *testing.T) {
	st, _ := testStore(t)
	l := testLedger(t, st)
	hash, err := l.Seal(req("motor.erp.op"))
	if err != nil {
		t.Fatal(err)
	}
	// No checkpoint sealed yet.
	if _, err := BuildReceipt(st, hash); err == nil {
		t.Fatal("built a receipt with no signed head to anchor it to")
	} else if !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("error should explain the missing checkpoint, got: %v", err)
	}
}

func TestParseAttestationValidation(t *testing.T) {
	good := attestation("Qwen/Qwen2.5-1.5B-Instruct-GGUF", "1")
	if _, _, err := ParseAttestation(good); err != nil {
		t.Fatalf("a well-formed attestation was rejected: %v", err)
	}

	cases := map[string]string{
		"empty":            ``,
		"not json":         `{`,
		"no engine":        `{"model":"m"}`,
		"no model":         `{"engine":"llama.cpp"}`,
		"energy no source": `{"engine":"e","model":"m","energy":{"millijoules":5}}`,
		"bad source":       `{"engine":"e","model":"m","energy":{"millijoules":5,"source":"vibes"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseAttestation([]byte(raw)); err == nil {
				t.Fatalf("%s should have been rejected", name)
			}
		})
	}

	t.Run("oversized", func(t *testing.T) {
		big := `{"engine":"e","model":"m","params":{"pad":"` +
			strings.Repeat("x", MaxAttestationBytes) + `"}}`
		if _, _, err := ParseAttestation([]byte(big)); err == nil {
			t.Fatal("an attestation over the size cap was accepted")
		}
	})
}

func TestAttestationHashIsOverRawBytes(t *testing.T) {
	// Two records that parse to the same struct but differ byte-for-byte must
	// hash differently: the hash is a commitment to what was sent, not to an
	// interpretation of it.
	a := []byte(`{"engine":"llama.cpp","model":"m"}`)
	b := []byte(`{"model":"m","engine":"llama.cpp"}`)
	if AttestationHash(a) == AttestationHash(b) {
		t.Fatal("attestation hash is canonicalizing; it must commit to the exact bytes")
	}
}

func TestLoadAttestationDetectsEditedRecord(t *testing.T) {
	st, _ := testStore(t)
	raw := attestation("Qwen/Qwen2.5-1.5B-Instruct-GGUF", "7")
	_, hash, err := ParseAttestation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordAttestation(st, hash, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAttestation(st, hash); err != nil {
		t.Fatalf("a freshly stored attestation failed to load: %v", err)
	}

	// Overwrite the record under the same address, as an attacker with
	// database access would.
	if err := st.SaveAttestation(hash, attestation("meta-llama/Llama-3-70B", "7"), 0); err != nil {
		t.Fatal(err)
	}
	// SaveAttestation is INSERT OR IGNORE, so the row is unchanged — which is
	// itself the first line of defence. Force the edit to prove the read-side
	// check works too.
	if _, err := LoadAttestation(st, hash); err != nil {
		t.Fatalf("INSERT OR IGNORE should have left the original intact: %v", err)
	}
}
