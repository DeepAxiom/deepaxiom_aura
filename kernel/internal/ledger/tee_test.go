package ledger

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The claim this file exists to make good on: a hardware quote attached to an
// attestation proves something about *that declaration*, not merely that the
// process was in an enclave. So every test below is a way of attaching evidence
// that should not count, and each has to come back at a level lower than it
// appears to claim.

// jwtQuote is a structurally valid EAT: three dot-separated segments.
func jwtQuote() string {
	return base64.StdEncoding.EncodeToString([]byte("header.payload.signature"))
}

// attestWithEvidence builds an attestation, computes its binding hash, and
// attaches evidence quoting that hash — the honest flow a skill performs.
func attestWithEvidence(t *testing.T, mutate func(*Evidence)) []byte {
	t.Helper()
	base := map[string]any{
		"engine":  "llama.cpp",
		"model":   "Qwen/Qwen2.5-1.5B-Instruct-GGUF",
		"quant":   "Q4_K_M",
		"ts":      time.Now().UnixMilli(),
		"seed":    42,
		"sampler": map[string]any{"temperature": 0.7},
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := BindingHash(raw)
	if err != nil {
		t.Fatalf("BindingHash: %v", err)
	}
	ev := Evidence{
		Format: FormatNvidiaEAT, Nonce: nonce, Quote: jwtQuote(),
		TS: base["ts"].(int64), Verifier: "NRAS",
	}
	if mutate != nil {
		mutate(&ev)
	}
	base["tee"] = ev
	full, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	return full
}

// The honest path: evidence quoting this record's own hash binds.
func TestEvidenceBindsWhenTheNonceIsThisRecordsHash(t *testing.T) {
	raw := attestWithEvidence(t, nil)
	rep := CheckEvidence(raw, TrustAnchors{})

	if rep.Level != EvidenceBound {
		t.Fatalf("level = %q (%s), want %q", rep.Level, rep.Detail, EvidenceBound)
	}
	if !rep.Bound {
		t.Error("evidence quoting this record's hash was not reported as bound")
	}
	// Bound is not verified, and the report has to say why rather than leaving
	// a reader to assume the stronger thing.
	if !strings.Contains(rep.Detail, "no trust anchor") {
		t.Errorf("a bound-but-unverified report should say the chain was unchecked; got %q", rep.Detail)
	}
}

// The attack the binding exists to stop: a genuine quote, taken from a
// different run, attached to this declaration.
func TestEvidenceFromAnotherRecordDoesNotBind(t *testing.T) {
	other := attestWithEvidence(t, nil)
	var otherObj map[string]json.RawMessage
	if err := json.Unmarshal(other, &otherObj); err != nil {
		t.Fatal(err)
	}
	stolen := otherObj["tee"]

	// A different declaration — a different model — carrying that evidence.
	mine := map[string]any{
		"engine": "llama.cpp",
		"model":  "some/other-model",
		"ts":     time.Now().UnixMilli(),
		"tee":    json.RawMessage(stolen),
	}
	raw, _ := json.Marshal(mine)

	rep := CheckEvidence(raw, TrustAnchors{})
	if rep.Bound || rep.Level != EvidenceNone {
		t.Fatalf("a quote taken for another declaration was accepted: level=%q bound=%v",
			rep.Level, rep.Bound)
	}
	if !strings.Contains(rep.Detail, "does not bind this record") {
		t.Errorf("the refusal should name the binding failure; got %q", rep.Detail)
	}
}

// Editing the declaration after the quote was taken must invalidate it — that
// is the same property, seen from the other side, and it is what stops a skill
// quoting an honest configuration and then shipping a different one.
func TestEditingTheRecordAfterQuotingBreaksTheBinding(t *testing.T) {
	raw := attestWithEvidence(t, nil)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	obj["model"] = json.RawMessage(`"a-model-nobody-quoted"`)
	edited, _ := json.Marshal(obj)

	rep := CheckEvidence(edited, TrustAnchors{})
	if rep.Bound {
		t.Fatal("a record edited after its quote was taken still reported as bound")
	}
}

// The binding hash must ignore the evidence block itself, or it would be
// self-referential and uncomputable.
func TestBindingHashIgnoresTheEvidenceBlock(t *testing.T) {
	without := []byte(`{"engine":"llama.cpp","model":"m","ts":1}`)
	with := []byte(`{"engine":"llama.cpp","model":"m","ts":1,"tee":{"format":"x"}}`)

	a, err := BindingHash(without)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BindingHash(with)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("the binding hash changed when the tee block was added (%s vs %s); "+
			"a skill could never compute it", shortNonce(a), shortNonce(b))
	}
}

// A skill in another language computes this from its own dict, so key order
// must not matter.
func TestBindingHashIsIndependentOfKeyOrder(t *testing.T) {
	a, err := BindingHash([]byte(`{"engine":"e","model":"m","ts":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := BindingHash([]byte(`{"ts":1,"model":"m","engine":"e"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("key order changed the binding hash; a Python skill and this kernel " +
			"would disagree on every record")
	}
}

// No evidence is the ordinary state and must not read as a failure.
func TestNoEvidenceIsNotAFailure(t *testing.T) {
	rep := CheckEvidence([]byte(`{"engine":"llama.cpp","model":"m"}`), TrustAnchors{})
	if rep.Level != EvidenceNone {
		t.Fatalf("level = %q, want %q", rep.Level, EvidenceNone)
	}
	if rep.Detail != "" {
		t.Errorf("a self-declared attestation produced a complaint: %q", rep.Detail)
	}
}

// A format this binary cannot check must be reported as unchecked, never
// accepted on the strength of its own name.
func TestUnknownFormatIsNotTrustedOnItsOwnSayS0(t *testing.T) {
	raw := attestWithEvidence(t, func(e *Evidence) { e.Format = "acme-super-tee/9" })
	rep := CheckEvidence(raw, TrustAnchors{})
	if rep.Level != EvidenceNone || rep.Bound {
		t.Fatalf("an unrecognised format was accepted: level=%q bound=%v", rep.Level, rep.Bound)
	}
	if !strings.Contains(rep.Detail, "cannot check it") {
		t.Errorf("the refusal should say the format is uncheckable; got %q", rep.Detail)
	}
}

func TestMalformedQuotesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Evidence)
		want   string
	}{
		{"empty quote", func(e *Evidence) { e.Quote = "" }, "no quote"},
		{"not base64", func(e *Evidence) { e.Quote = "!!!not base64!!!" }, "not base64"},
		{"nonce not hex", func(e *Evidence) { e.Nonce = "zzzz" }, "not hex"},
		{"no nonce", func(e *Evidence) { e.Nonce = "" }, "carries no nonce"},
		{"no timestamp", func(e *Evidence) { e.TS = 0 }, "no timestamp"},
		{"not a JWT", func(e *Evidence) {
			e.Quote = base64.StdEncoding.EncodeToString([]byte("no-dots-here"))
		}, "not a JWT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := CheckEvidence(attestWithEvidence(t, tc.mutate), TrustAnchors{})
			if rep.Bound {
				t.Fatalf("%s was accepted as bound", tc.name)
			}
			if !strings.Contains(rep.Detail, tc.want) {
				t.Errorf("detail %q does not mention %q", rep.Detail, tc.want)
			}
		})
	}
}

// A quote taken long before the inference it claims to attest is refused even
// though the binding would hold — the binding already makes reuse impossible,
// and this is the second line that makes a confused implementation visible.
func TestAStaleQuoteIsRefused(t *testing.T) {
	raw := attestWithEvidence(t, func(e *Evidence) {
		e.TS = time.Now().Add(-2 * time.Hour).UnixMilli()
	})
	rep := CheckEvidence(raw, TrustAnchors{})
	if rep.Bound {
		t.Fatal("a quote taken two hours from its inference was accepted")
	}
	if !strings.Contains(rep.Detail, "from the inference it attests") {
		t.Errorf("the refusal should name the staleness; got %q", rep.Detail)
	}
}

// The single most damaging possible bug: reporting `verified` without having
// verified anything. Chain verification is declared and refused in this build,
// and a supplied anchor must not by itself promote the level.
func TestAnAnchorAloneDoesNotProduceVerified(t *testing.T) {
	raw := attestWithEvidence(t, nil)
	anchors := TrustAnchors{Roots: map[string][]byte{
		FormatNvidiaEAT: []byte("-----BEGIN CERTIFICATE-----\nnot a real root\n-----END CERTIFICATE-----"),
	}}
	rep := CheckEvidence(raw, anchors)

	if rep.Level == EvidenceVerified {
		t.Fatal("a node claimed `verified` on the strength of chain code that has never " +
			"run against hardware")
	}
	if rep.Level != EvidenceBound {
		t.Fatalf("level = %q, want %q — the binding still holds", rep.Level, EvidenceBound)
	}
	if !strings.Contains(rep.Detail, "not implemented in this build") {
		t.Errorf("the report should say plainly what was not done; got %q", rep.Detail)
	}
}

// Evidence larger than the cap must be refused rather than parsed, since it
// rides inside a record that is itself bounded.
func TestOversizedEvidenceIsRefused(t *testing.T) {
	big := strings.Repeat("A", MaxEvidenceBytes+64)
	raw := []byte(`{"engine":"e","model":"m","tee":{"format":"nvidia-eat/1","quote":"` + big + `"}}`)
	rep := CheckEvidence(raw, TrustAnchors{})
	if rep.Bound || !strings.Contains(rep.Detail, "over the") {
		t.Fatalf("oversized evidence was not refused: %+v", rep)
	}
}
