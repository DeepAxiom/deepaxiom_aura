package ledger

import "testing"

// C5 v1.2: usage is optional, additive, and refused only when it is nonsense.
func TestAttestationUsage(t *testing.T) {
	with := []byte(`{"engine":"llama.cpp","model":"m","usage":{"input_tokens":900,"output_tokens":40,"cached_input_tokens":768}}`)
	a, hash, err := ParseAttestation(with)
	if err != nil {
		t.Fatalf("a well-formed usage block was refused: %v", err)
	}
	if a.Usage == nil || *a.Usage.InputTokens != 900 || *a.Usage.OutputTokens != 40 || *a.Usage.CachedInputTokens != 768 {
		t.Fatalf("usage parsed as %+v", a.Usage)
	}

	// A record without the block is exactly what it was before v1.2 — same
	// hash input, same hash — so nothing already sealed changes meaning.
	without := []byte(`{"engine":"llama.cpp","model":"m"}`)
	b, bareHash, err := ParseAttestation(without)
	if err != nil || b.Usage != nil {
		t.Fatalf("a record with no usage: err=%v usage=%+v", err, b.Usage)
	}
	if bareHash != AttestationHash(without) || bareHash == hash {
		t.Fatal("content addressing changed for a record that carries no usage")
	}

	// Different counts are different inferences.
	other := []byte(`{"engine":"llama.cpp","model":"m","usage":{"input_tokens":901,"output_tokens":40,"cached_input_tokens":768}}`)
	if _, otherHash, _ := ParseAttestation(other); otherHash == hash {
		t.Fatal("two attestations with different usage share a hash")
	}

	if _, _, err := ParseAttestation([]byte(`{"engine":"e","model":"m","usage":{"output_tokens":-3}}`)); err == nil {
		t.Fatal("a negative token count was accepted")
	}
	// A partial report is fine: an engine that only counts output says so.
	if _, _, err := ParseAttestation([]byte(`{"engine":"e","model":"m","usage":{"output_tokens":3}}`)); err != nil {
		t.Fatalf("a partial usage report was refused: %v", err)
	}
}
