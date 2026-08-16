package ledger

import (
	"strings"
	"testing"
)

// The aggregation has one job that matters: not lying about what changed. A
// regression report that overstates makes people mute it; one that understates
// is worse than nothing. These pin both edges.

func entryFor(capability, decision, outcome string, models ...string) Entry {
	return Entry{
		Capability: capability, Decision: decision, Outcome: outcome,
		Actor: "acme/motor/x@1.0.0", Policy: "sha256:p", Inference: models,
	}
}

func resultFor(session string, old, new []Entry) SessionResult {
	return SessionResult{
		Session: session, Replay: session + "-replay",
		Diff:      Diff(old, new),
		OldModels: ModelsOf(old), NewModels: ModelsOf(new),
	}
}

func TestRegressReportsACleanRun(t *testing.T) {
	same := []Entry{entryFor("motor.api.crm.create", "gate", "delivered")}
	r := Regress([]SessionResult{
		resultFor("s1", same, same),
		resultFor("s2", same, same),
	})
	if !r.Clean() {
		t.Fatalf("identical runs reported a regression: %+v", r.Divergent())
	}
	if r.Reproducible != 2 || r.Regressed != 0 || r.Failed != 0 {
		t.Errorf("counts = %d reproduced, %d regressed, %d failed", r.Reproducible, r.Regressed, r.Failed)
	}
	if r.EffectsBefore != 2 || r.EffectsAfter != 2 {
		t.Errorf("effects = %d before, %d after", r.EffectsBefore, r.EffectsAfter)
	}
}

// The finding an output diff structurally cannot make. An effect that stopped
// happening leaves no text behind to compare, so it reads as silence unless
// something is counting the effects themselves.
func TestRegressCatchesAnEffectThatStoppedHappening(t *testing.T) {
	before := []Entry{
		entryFor("motor.api.crm.create", "gate", "delivered"),
		entryFor("motor.payments.refund", "gate", "delivered"),
	}
	after := []Entry{entryFor("motor.api.crm.create", "gate", "delivered")}

	r := Regress([]SessionResult{resultFor("s1", before, after)})
	if r.Clean() {
		t.Fatal("a disappearing effect was reported as reproducible")
	}
	if r.EffectsBefore != 2 || r.EffectsAfter != 1 {
		t.Errorf("effects = %d before, %d after", r.EffectsBefore, r.EffectsAfter)
	}
	if r.Regressed != 1 {
		t.Errorf("regressed = %d, want 1", r.Regressed)
	}
}

// A session that could not be replayed is not evidence that anything changed.
// Counting it as a regression would make one offline skill look like a
// behavioural change, which is how a report stops being believed.
func TestAFailedReplayIsNotARegression(t *testing.T) {
	same := []Entry{entryFor("motor.api.crm.create", "gate", "delivered")}
	r := Regress([]SessionResult{
		resultFor("s1", same, same),
		{Session: "s2", Error: "graph no longer registered"},
	})
	if !r.Clean() {
		t.Error("a session that could not run was counted as a regression")
	}
	if r.Failed != 1 || r.Reproducible != 1 {
		t.Errorf("failed = %d, reproduced = %d", r.Failed, r.Reproducible)
	}
	if r.Sessions != 2 {
		t.Errorf("sessions = %d", r.Sessions)
	}
	// The verdict has to admit the gap rather than reading as a clean pass.
	if v := r.Verdict(); v == "" || !contains(v, "could not be replayed") {
		t.Errorf("verdict hides the incomplete coverage: %q", v)
	}
}

func TestRegressGroupsByCapability(t *testing.T) {
	before := []Entry{entryFor("motor.payments.refund", "gate", "delivered")}
	afterDenied := []Entry{entryFor("motor.payments.refund", "gate", "denied")}

	r := Regress([]SessionResult{
		resultFor("s1", before, afterDenied),
		resultFor("s2", before, afterDenied),
		resultFor("s3", before, before),
	})
	if len(r.ByCapability) != 1 {
		t.Fatalf("by-capability = %+v, want exactly one entry", r.ByCapability)
	}
	got := r.ByCapability[0]
	if got.Sessions != 2 {
		t.Errorf("sessions for %s = %d, want 2", got.Capability, got.Sessions)
	}
	if got.Fields["outcome"] != 2 {
		t.Errorf("outcome divergences = %d, want 2", got.Fields["outcome"])
	}
}

// The differentiator: the report says which two configurations were compared,
// not just that something moved.
func TestRegressCollectsTheModelsOnEachSide(t *testing.T) {
	before := []Entry{entryFor("motor.api.crm.create", "gate", "delivered", "sha256:old")}
	after := []Entry{entryFor("motor.api.crm.create", "gate", "delivered", "sha256:new")}

	r := Regress([]SessionResult{resultFor("s1", before, after)})
	if len(r.ModelsBefore) != 1 || r.ModelsBefore[0] != "sha256:old" {
		t.Errorf("models before = %v", r.ModelsBefore)
	}
	if len(r.ModelsAfter) != 1 || r.ModelsAfter[0] != "sha256:new" {
		t.Errorf("models after = %v", r.ModelsAfter)
	}
	// Citing a different model is not itself a regression — that is the
	// premise of the run, not its finding.
	if !r.Clean() {
		t.Error("changing the cited model was reported as a behavioural change")
	}
}

func TestModelsOfDeduplicatesAndSorts(t *testing.T) {
	got := ModelsOf([]Entry{
		entryFor("a", "allow", "delivered", "sha256:b", "sha256:a"),
		entryFor("b", "allow", "delivered", "sha256:a"),
	})
	if len(got) != 2 || got[0] != "sha256:a" || got[1] != "sha256:b" {
		t.Errorf("models = %v, want [sha256:a sha256:b]", got)
	}
	if ModelsOf(nil) != nil {
		t.Error("no entries should cite no models")
	}
}

func TestEmptyRunIsReportedAsSuch(t *testing.T) {
	r := Regress(nil)
	if !r.Clean() {
		t.Error("an empty run is not a failure")
	}
	if !contains(r.Verdict(), "no sessions") {
		t.Errorf("verdict = %q", r.Verdict())
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
