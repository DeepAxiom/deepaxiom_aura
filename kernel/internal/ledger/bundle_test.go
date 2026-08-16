package ledger

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"aura/kernel/internal/store"
)

// sessionWithHistory writes a realistic trajectory and seals effects against
// it, so a bundle has something to describe.
func sessionWithHistory(t *testing.T, steps int) (*store.Store, *Ledger, string) {
	t.Helper()
	st, _ := testStore(t)
	l := testLedger(t, st)
	const session = "sess-bundle"

	raw := attestation("Qwen/Qwen2.5-1.5B-Instruct-GGUF", "42")
	_, attHash, err := ParseAttestation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordAttestation(st, attHash, raw); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < steps; i++ {
		env, _ := json.Marshal(map[string]any{
			"v": "1", "id": fmt.Sprintf("ENV-%d", i), "cause_id": "ENV-0",
			"session": session, "kind": "data",
			"payload": map[string]any{"text": fmt.Sprintf("step %d", i)},
		})
		if err := st.AppendEvent(session, fmt.Sprintf("ENV-%d", i), "ENV-0", "data", env); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 3; i++ {
		r := req("motor.erp.invoice.create")
		r.Session = session
		r.Envelope = fmt.Sprintf("ENV-%d", i)
		r.Inference = []string{attHash}
		if _, err := l.Seal(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	return st, l, session
}

func TestBundleVerifiesStandalone(t *testing.T) {
	st, _, session := sessionWithHistory(t, 12)

	b, err := BuildBundle(st, session)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}

	// Through JSON — and through MarshalIndent specifically, which is what the
	// CLI writes and what broke the receipt the first time around.
	blob, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var portable Bundle
	if err := json.Unmarshal(blob, &portable); err != nil {
		t.Fatal(err)
	}

	rep := VerifyBundle(portable)
	if !rep.Sound() {
		t.Fatalf("a freshly built bundle did not verify: %+v", rep)
	}
	if rep.Steps != 12 {
		t.Fatalf("bundle carries %d steps, want 12", rep.Steps)
	}
	if rep.Effects != 3 || rep.EffectsSound != 3 {
		t.Fatalf("expected 3/3 verifiable effects, got %d/%d", rep.EffectsSound, rep.Effects)
	}
	if rep.Attestations != 1 || rep.AttestationsSound != 1 {
		t.Fatalf("expected 1/1 model configurations, got %d/%d",
			rep.AttestationsSound, rep.Attestations)
	}

	// The four materials the audit-bundle result asks a runtime to emit.
	if len(portable.Trajectory) == 0 {
		t.Error("no trajectory")
	}
	if len(portable.Effects) == 0 {
		t.Error("no artifact provenance")
	}
	if len(portable.Attestations) == 0 {
		t.Error("no model configuration")
	}
	if portable.Summary.Effects != 3 || portable.Summary.EffectsDelivered != 3 {
		t.Errorf("summary miscounts effects: %+v", portable.Summary)
	}
}

// A bundle whose trajectory was edited must not verify — that is the whole
// point of carrying a hash over it.
func TestBundleDetectsAnEditedTrajectory(t *testing.T) {
	st, _, session := sessionWithHistory(t, 6)
	base, err := BuildBundle(st, session)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("altered step", func(t *testing.T) {
		b := base
		b.Trajectory = append([]Step(nil), base.Trajectory...)
		forged, _ := json.Marshal(map[string]any{
			"v": "1", "id": "ENV-2", "session": session, "kind": "data",
			"payload": map[string]any{"text": "something the agent never did"},
		})
		b.Trajectory[2].Envelope = base64.StdEncoding.EncodeToString(forged)
		rep := VerifyBundle(b)
		if rep.Sound() || rep.TrajectoryIntact {
			t.Fatal("an edited trajectory verified")
		}
	})

	t.Run("dropped step", func(t *testing.T) {
		b := base
		b.Trajectory = append(append([]Step(nil), base.Trajectory[:2]...), base.Trajectory[3:]...)
		if VerifyBundle(b).Sound() {
			t.Fatal("a trajectory with a step removed verified — a benchmark could " +
				"delete the turn that shows how a score was earned")
		}
	})

	t.Run("reordered steps", func(t *testing.T) {
		b := base
		b.Trajectory = append([]Step(nil), base.Trajectory...)
		b.Trajectory[1], b.Trajectory[2] = b.Trajectory[2], b.Trajectory[1]
		if VerifyBundle(b).Sound() {
			t.Fatal("a reordered trajectory verified")
		}
	})
}

func TestBundleDetectsASwappedModelConfiguration(t *testing.T) {
	st, _, session := sessionWithHistory(t, 5)
	b, err := BuildBundle(st, session)
	if err != nil {
		t.Fatal(err)
	}
	for hash := range b.Attestations {
		b.Attestations[hash] = base64.StdEncoding.EncodeToString(
			attestation("meta-llama/Llama-3-70B", "42"))
	}
	rep := VerifyBundle(b)
	if rep.Sound() || rep.AttestationsSound != 0 {
		t.Fatal("a bundle claiming a different model than the one bound to its effects verified")
	}
}

// Truncation is honest, not fatal: a prefix of a real session is still
// auditable material, and the bundle says it is a prefix.
func TestBundleTruncationIsDeclaredNotHidden(t *testing.T) {
	st, _, session := sessionWithHistory(t, MaxTrajectorySteps+50)
	b, err := BuildBundle(st, session)
	if err != nil {
		t.Fatal(err)
	}
	if !b.TrajectoryTruncated {
		t.Fatal("an over-long session produced a bundle that did not declare truncation")
	}
	if len(b.Trajectory) != MaxTrajectorySteps {
		t.Fatalf("carried %d steps, want the cap of %d", len(b.Trajectory), MaxTrajectorySteps)
	}
	rep := VerifyBundle(b)
	if !rep.Sound() {
		t.Fatalf("a truncated but intact bundle failed: %+v", rep)
	}
	if !rep.Truncated {
		t.Fatal("the report did not carry truncation through to the reader")
	}
}

func TestBundleRefusesAnEmptySession(t *testing.T) {
	st, _ := testStore(t)
	if _, err := BuildBundle(st, "sess-nothing"); err == nil {
		t.Fatal("built a bundle for a session with no recorded events")
	}
}
