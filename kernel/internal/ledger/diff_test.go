package ledger

import "testing"

// entry builds a minimal, valid-looking sealed entry for Diff's table tests.
// Diff never touches Seq/Prev/TS/Node/Session/Envelope/Cause, so they are
// left zero-valued — only the fields Diff actually compares matter here.
func entry(capability, decision, outcome string) Entry {
	return Entry{
		Actor: "acme/motor/writer@1.0.0", Capability: capability,
		Decision: decision, Outcome: outcome,
		Policy: "sha256:policyhash", PayloadSHA256: "deadbeef",
	}
}

func TestDiffOfIdenticalSlicesIsReproducibleWithNoDivergences(t *testing.T) {
	old := []Entry{entry("motor.erp.write", "allow", "delivered"), entry("motor.tts.speak", "gate", "delivered")}
	newE := make([]Entry, len(old))
	copy(newE, old)

	r := Diff(old, newE)
	if !r.Reproducible() {
		t.Fatalf("identical slices reported divergences: %+v", r.Divergences)
	}
	if len(r.Notes) != 0 {
		t.Fatalf("identical slices reported notes: %+v", r.Notes)
	}
	if r.Compared != 2 || r.OldCount != 2 || r.NewCount != 2 {
		t.Fatalf("counts = %+v, want old=2 new=2 compared=2", r)
	}
}

// The core scenario this exists to catch: the policy in force changed
// between the original run and replay, and the same capability now resolves
// to a different decision.
func TestDiffCatchesADecisionThatFlipped(t *testing.T) {
	old := []Entry{entry("motor.erp.write", "allow", "delivered")}
	newE := []Entry{entry("motor.erp.write", "gate", "delivered")}

	r := Diff(old, newE)
	if r.Reproducible() {
		t.Fatal("a decision flip (allow -> gate) was not reported as a divergence")
	}
	if len(r.Divergences) != 1 || r.Divergences[0].Field != "decision" {
		t.Fatalf("divergences = %+v, want exactly one on field=decision", r.Divergences)
	}
	if r.Divergences[0].Old != "allow" || r.Divergences[0].New != "gate" {
		t.Fatalf("divergence = %+v, want old=allow new=gate", r.Divergences[0])
	}
}

func TestDiffCatchesACapabilityMismatch(t *testing.T) {
	old := []Entry{entry("motor.erp.write", "allow", "delivered")}
	newE := []Entry{entry("motor.payments.send", "allow", "delivered")}

	r := Diff(old, newE)
	if r.Reproducible() {
		t.Fatal("a different capability at the same position was not reported")
	}
	if len(r.Divergences) != 1 || r.Divergences[0].Field != "capability" {
		t.Fatalf("divergences = %+v, want exactly one on field=capability", r.Divergences)
	}
}

func TestDiffReportsALengthMismatchAndStillComparesTheOverlap(t *testing.T) {
	old := []Entry{
		entry("motor.erp.write", "allow", "delivered"),
		entry("motor.tts.speak", "allow", "delivered"),
	}
	newE := []Entry{entry("motor.erp.write", "allow", "delivered")}

	r := Diff(old, newE)
	if r.OldCount != 2 || r.NewCount != 1 {
		t.Fatalf("counts = old=%d new=%d, want old=2 new=1", r.OldCount, r.NewCount)
	}
	if r.Compared != 1 {
		t.Fatalf("compared = %d, want 1 (the overlapping prefix)", r.Compared)
	}
	found := false
	for _, d := range r.Divergences {
		if d.Field == "count" {
			found = true
			if d.Old != "2" || d.New != "1" {
				t.Fatalf("count divergence = %+v, want old=2 new=1", d)
			}
		}
	}
	if !found {
		t.Fatalf("no count divergence reported: %+v", r.Divergences)
	}
	// The one overlapping entry matched on every hard field, so it must not
	// also produce a spurious divergence beyond the length mismatch itself.
	if len(r.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want exactly the one count divergence", r.Divergences)
	}
}

// A model-backed skill wording an effect differently is not an authorization
// failure — payload_sha256 differing lands in Notes, not Divergences.
func TestDiffPutsAPayloadDifferenceInNotesNotDivergences(t *testing.T) {
	old := entry("motor.erp.write", "allow", "delivered")
	newE := entry("motor.erp.write", "allow", "delivered")
	newE.PayloadSHA256 = "somethingdifferent"

	r := Diff([]Entry{old}, []Entry{newE})
	if !r.Reproducible() {
		t.Fatalf("a payload-only difference was reported as a divergence: %+v", r.Divergences)
	}
	if len(r.Notes) != 1 || r.Notes[0].Field != "payload_sha256" {
		t.Fatalf("notes = %+v, want exactly one on field=payload_sha256", r.Notes)
	}
}

// A skill version upgrade and a policy document update are both expected to
// vary run to run and belong in Notes, not Divergences.
func TestDiffPutsActorAndPolicyDifferencesInNotes(t *testing.T) {
	old := entry("motor.erp.write", "allow", "delivered")
	newE := entry("motor.erp.write", "allow", "delivered")
	newE.Actor = "acme/motor/writer@1.1.0"
	newE.Policy = "sha256:differentpolicyhash"

	r := Diff([]Entry{old}, []Entry{newE})
	if !r.Reproducible() {
		t.Fatalf("actor/policy differences were reported as divergences: %+v", r.Divergences)
	}
	fields := map[string]bool{}
	for _, n := range r.Notes {
		fields[n.Field] = true
	}
	if !fields["actor"] || !fields["policy"] {
		t.Fatalf("notes = %+v, want both actor and policy", r.Notes)
	}
}

// A skill that gained or lost a declared `compensates` between runs changed
// something worth surfacing, but not an authorization outcome.
func TestDiffPutsACompensationShapeChangeInNotes(t *testing.T) {
	old := entry("motor.erp.write", "allow", "delivered")
	newE := entry("motor.erp.write", "allow", "delivered")
	newE.Compensation = &Compensation{Capability: "motor.erp.write", Port: "undo_in"}

	r := Diff([]Entry{old}, []Entry{newE})
	if !r.Reproducible() {
		t.Fatalf("a compensation shape change was reported as a divergence: %+v", r.Divergences)
	}
	if len(r.Notes) != 1 || r.Notes[0].Field != "compensation" {
		t.Fatalf("notes = %+v, want exactly one on field=compensation", r.Notes)
	}
}

func TestDiffOfTwoEmptySlicesIsReproducibleAndDoesNotPanic(t *testing.T) {
	r := Diff(nil, nil)
	if !r.Reproducible() {
		t.Fatalf("empty vs empty reported divergences: %+v", r.Divergences)
	}
	if r.Compared != 0 || r.OldCount != 0 || r.NewCount != 0 {
		t.Fatalf("counts = %+v, want all zero", r)
	}
}
