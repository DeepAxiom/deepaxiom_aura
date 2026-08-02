package ledger

// The audit bundle — everything needed to re-examine one session, in one file.
//
// # Why this shape
//
// A 2026 result on agent evaluation ("Do Agent Benchmarks Measure Capability?",
// arXiv 2607.22368) found that 67% of examined benchmark traces contained
// "protocol exposures" — paths by which a score could be earned without the
// capability being measured. Its proposed remedy is an **audit bundle**: the
// retained materials that let a second reader reproduce an attribution rather
// than take a number on faith. The paper names what a runtime has to emit:
//
//	complete trajectory logs   tool calls, ordering, timestamps
//	artifact provenance        what was produced, with hashes
//	model configuration        replayable: which model, which parameters, seed
//	comparison baselines       paired runs, so a claim can be contrasted
//
// A runtime that emits those four is a runtime whose results can be audited.
// Almost nothing emits them, which is why agent evaluation has a
// reproducibility problem rather than a tooling problem.
//
// This kernel already produced all four for unrelated reasons: the causal
// event log (C3 rule 7) is the trajectory, the effect ledger (C4) is artifact
// provenance with hashes, the inference attestation (C5) is the model
// configuration, and `aura replay` produces paired runs. What was missing was
// a single document that carries them together and verifies standalone.
//
// # Relationship to the receipt
//
// A receipt (receipt.go) proves one effect. A bundle explains one session: it
// carries a receipt per sealed effect, plus the trajectory those effects sit
// in and the attestations that argued for them. The receipt is evidence; the
// bundle is the case file.
//
// Both verify with no database, no node and no network.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"aura/kernel/internal/store"
)

// BundleVersion identifies the document shape.
const BundleVersion = "aura-audit-bundle/1"

// MaxTrajectorySteps bounds how many envelopes a bundle carries.
//
// A long voice session emits tens of thousands; a bundle is meant to be read
// and shared, and one that needs streaming to open is one nobody opens. When
// the cap bites the bundle says so explicitly rather than appearing complete —
// a truncated trajectory presented as whole would reintroduce exactly the
// "score without evidence" problem the format exists to solve.
const MaxTrajectorySteps = 5000

// Bundle is the case file for one session.
type Bundle struct {
	Version string `json:"version"`
	Session string `json:"session"`
	Graph   string `json:"graph"`
	Node    string `json:"node"`

	// Trajectory is the session's causal event log in order: every envelope
	// the kernel routed, with its cause and its timestamp. Base64 for the same
	// reason the receipt's entry is — see receipt.go — since a step's hash is
	// over the bytes as recorded.
	Trajectory []Step `json:"trajectory"`
	// TrajectoryTruncated is true when the session had more steps than
	// MaxTrajectorySteps and this bundle carries a prefix.
	TrajectoryTruncated bool `json:"trajectory_truncated,omitempty"`
	// TrajectoryHash commits to the whole trajectory, truncated or not, so two
	// bundles of the same session are comparable by one value.
	TrajectoryHash string `json:"trajectory_hash"`

	// Effects are the sealed C4 entries this session produced, each as a
	// standalone receipt that verifies on its own.
	Effects []Receipt `json:"effects,omitempty"`

	// Attestations are the C5 records cited anywhere in this session, keyed by
	// content address — the model configuration half of the audit bundle.
	Attestations map[string]string `json:"attestations,omitempty"`

	// Summary is the at-a-glance accounting a reader sees first.
	Summary BundleSummary `json:"summary"`
}

// Step is one envelope in the trajectory.
type Step struct {
	Seq      int    `json:"seq"`
	TS       int64  `json:"ts"`
	Envelope string `json:"envelope_b64"`
}

// BundleSummary is the header a reader checks before reading anything else.
type BundleSummary struct {
	Steps            int      `json:"steps"`
	Effects          int      `json:"effects"`
	EffectsDelivered int      `json:"effects_delivered"`
	EffectsDenied    int      `json:"effects_denied"`
	Models           []string `json:"models,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
	Policies         []string `json:"policies,omitempty"`
	EnergyMJ         float64  `json:"energy_millijoules,omitempty"`
	EnergySources    []string `json:"energy_sources,omitempty"`
	StartedTS        int64    `json:"started_ts,omitempty"`
	EndedTS          int64    `json:"ended_ts,omitempty"`
	GeneratedTS      int64    `json:"generated_ts"`
}

// BuildBundle assembles the audit bundle for one session.
func BuildBundle(st *store.Store, session string) (Bundle, error) {
	raw, timestamps, err := st.SessionEvents(session, MaxTrajectorySteps+1)
	if err != nil {
		return Bundle{}, fmt.Errorf("read trajectory: %w", err)
	}
	if len(raw) == 0 {
		return Bundle{}, fmt.Errorf(
			"session %q has no recorded events — a bundle is built from what actually "+
				"ran, so there is nothing to audit", session)
	}

	b := Bundle{
		Version: BundleVersion, Session: session,
		Summary: BundleSummary{GeneratedTS: time.Now().UnixMilli()},
	}
	if len(raw) > MaxTrajectorySteps {
		b.TrajectoryTruncated = true
		raw = raw[:MaxTrajectorySteps]
		timestamps = timestamps[:MaxTrajectorySteps]
	}

	// The trajectory hash covers the recorded bytes in order, so a reader can
	// tell two bundles of the same session apart by one comparison.
	h := sha256.New()
	b.Trajectory = make([]Step, len(raw))
	for i, env := range raw {
		h.Write(env)
		b.Trajectory[i] = Step{
			Seq: i + 1, TS: timestamps[i],
			Envelope: base64.StdEncoding.EncodeToString(env),
		}
	}
	b.TrajectoryHash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	b.Summary.Steps = len(b.Trajectory)
	if len(timestamps) > 0 {
		b.Summary.StartedTS = timestamps[0]
		b.Summary.EndedTS = timestamps[len(timestamps)-1]
	}

	// Every sealed effect, as a receipt that stands on its own.
	entries, err := st.LedgerEntriesBySession(session)
	if err != nil {
		return Bundle{}, fmt.Errorf("read sealed effects: %w", err)
	}
	models := map[string]bool{}
	capabilities := map[string]bool{}
	policies := map[string]bool{}
	energySources := map[string]bool{}

	for _, entryRaw := range entries {
		var e Entry
		if json.Unmarshal(entryRaw, &e) != nil {
			continue
		}
		b.Node = e.Node
		capabilities[e.Capability] = true
		policies[e.Policy] = true
		b.Summary.Effects++
		if e.Outcome == "denied" {
			b.Summary.EffectsDenied++
		} else {
			b.Summary.EffectsDelivered++
		}

		// A receipt needs a checkpoint covering it. An effect sealed since the
		// last one has none yet, which is a timing fact rather than a failure:
		// the effect is still in the trajectory and still in the ledger, it
		// simply cannot carry a standalone proof yet.
		if r, err := BuildReceipt(st, e.Hash()); err == nil {
			b.Effects = append(b.Effects, r)
		}

		for _, hash := range e.Inference {
			record, err := st.Attestation(hash)
			if err != nil {
				continue
			}
			if b.Attestations == nil {
				b.Attestations = map[string]string{}
			}
			b.Attestations[hash] = base64.StdEncoding.EncodeToString(record)

			var a Attestation
			if json.Unmarshal(record, &a) != nil {
				continue
			}
			label := a.Model
			if a.Quantization != "" {
				label += " (" + a.Quantization + ")"
			}
			if a.ModelRevision != "" {
				label += " @" + short(a.ModelRevision, 12)
			}
			models[label] = true
			if a.Energy != nil {
				b.Summary.EnergyMJ += a.Energy.Millijoules
				energySources[a.Energy.Source] = true
			}
		}
	}

	b.Summary.Models = sortedLabels(models)
	b.Summary.Capabilities = sortedLabels(capabilities)
	b.Summary.Policies = sortedLabels(policies)
	b.Summary.EnergySources = sortedLabels(energySources)
	return b, nil
}

// BundleReport is what verifying a bundle concluded.
type BundleReport struct {
	TrajectoryIntact bool `json:"trajectory_intact"`
	Steps            int  `json:"steps"`
	// EffectsSound / Effects: how many carried receipts verify standalone.
	Effects      int `json:"effects"`
	EffectsSound int `json:"effects_sound"`
	// AttestationsSound / Attestations: how many model-configuration records
	// match their content address.
	Attestations      int `json:"attestations"`
	AttestationsSound int `json:"attestations_sound"`
	// Truncated is carried through so a reader is never told a partial
	// trajectory was complete.
	Truncated bool `json:"truncated"`

	Problems []string `json:"problems,omitempty"`
}

// Sound is the conjunction of the checks a bundle must pass to be evidence.
//
// A truncated trajectory does not fail: it is honest about being a prefix, and
// a prefix of a real session is still auditable material. What fails is a
// trajectory whose bytes do not hash to what the bundle claims, or a receipt
// that does not verify — those are documents claiming something they cannot
// support.
func (r BundleReport) Sound() bool {
	return r.TrajectoryIntact &&
		r.EffectsSound == r.Effects &&
		r.AttestationsSound == r.Attestations
}

// VerifyBundle checks a bundle using nothing but the bundle.
func VerifyBundle(b Bundle) BundleReport {
	rep := BundleReport{Steps: len(b.Trajectory), Truncated: b.TrajectoryTruncated}

	if b.Version != BundleVersion {
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"bundle version %q, this verifier speaks %q", b.Version, BundleVersion))
	}

	// 1. The trajectory is the one the bundle commits to.
	h := sha256.New()
	for i, step := range b.Trajectory {
		env, err := base64.StdEncoding.DecodeString(step.Envelope)
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("step %d is not valid base64", i+1))
			return rep
		}
		if !json.Valid(env) {
			rep.Problems = append(rep.Problems, fmt.Sprintf("step %d is not a valid envelope", i+1))
			return rep
		}
		h.Write(env)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got == b.TrajectoryHash {
		rep.TrajectoryIntact = true
	} else {
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"trajectory hashes to %s but the bundle claims %s — steps were added, "+
				"removed or reordered", shortHash(got), shortHash(b.TrajectoryHash)))
	}

	// 2. Each carried effect proves itself.
	rep.Effects = len(b.Effects)
	for _, receipt := range b.Effects {
		sub := VerifyReceipt(receipt)
		if sub.Sound() {
			rep.EffectsSound++
			continue
		}
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"effect %s does not verify: %v", shortHash(receipt.EntryHash), sub.Problems))
	}

	// 3. Each model-configuration record matches its content address.
	rep.Attestations = len(b.Attestations)
	for hash, encoded := range b.Attestations {
		record, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("attestation %s is not valid base64", shortHash(hash)))
			continue
		}
		if AttestationHash(record) != hash {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"attestation %s does not match its content address — the record was "+
					"altered", shortHash(hash)))
			continue
		}
		rep.AttestationsSound++
	}
	return rep
}

// sortedLabels is sortedSet (bom.go) with empty values dropped — a summary
// listing a blank model name reads as a bug rather than as an absence.
func sortedLabels(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
