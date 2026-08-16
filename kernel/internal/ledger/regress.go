package ledger

import (
	"fmt"
	"sort"
)

// Regression testing against the ledger, across many sessions at once.
//
// diff.go answers "did replaying this one session reproduce what it recorded".
// That is the right unit for debugging and the wrong unit for the question
// teams actually face every week, which is not about one session at all:
//
//	we are about to change the model. What does that do to the effects?
//
// Nothing available today answers it with evidence. Eval suites score outputs
// against a rubric, which measures whether the text got better and says nothing
// about whether the runtime went on to charge a different card. Observability
// records what happened after the fact. Neither can say "these 340 sessions
// sealed the same effects; these 6 did not; here are the 6."
//
// This runtime can, and it is the one thing here nobody else is positioned to
// build — not because the aggregation is clever (it is a fold over Diff) but
// because of what it folds over. An effect ledger that cites the model
// revision, quantization and sampling parameters behind every act (C5) means
// the comparison is between two named, pinned configurations rather than
// between "before" and "after". A divergence comes with the two attestations
// that produced it.
//
// Deliberately pure, like diff.go: no store, no HTTP, no kernel. `aura regress`
// supplies the replayed pairs and this decides what they mean.

// SessionResult is one replayed session's outcome.
type SessionResult struct {
	// Session is the original session id; Replay is the id of the session the
	// replay ran under, so a divergence can be opened in `aura why`.
	Session string `json:"session"`
	Replay  string `json:"replay,omitempty"`
	// Error is set when the replay could not be carried out at all — the graph
	// is gone, a skill it needs is offline. Distinct from a divergence: a
	// session that could not run is not evidence that anything changed, and
	// counting it as a regression would make an offline skill look like a
	// behavioural change.
	Error string     `json:"error,omitempty"`
	Diff  DiffReport `json:"diff"`
	// Models are the attestation hashes cited by the old and new runs. What
	// makes the report answer "the model changed and here is what it did"
	// rather than only "something changed".
	OldModels []string `json:"old_models,omitempty"`
	NewModels []string `json:"new_models,omitempty"`
}

// Regressed reports whether this session's authorization behaviour changed. A
// session that failed to replay is not regressed — it is unknown, and the
// report counts it separately.
func (s SessionResult) Regressed() bool {
	return s.Error == "" && !s.Diff.Reproducible()
}

// CapabilityDelta summarises what changed for one capability across the whole
// run — the level at which somebody decides whether to ship.
//
// Per-capability rather than per-session because that is the unit an owner
// exists for. "Six sessions diverged" prompts a spreadsheet; "every divergence
// was in motor.payments.refund" prompts a decision.
type CapabilityDelta struct {
	Capability string `json:"capability"`
	Sessions   int    `json:"sessions"`
	// Fields counts divergences by kind (capability|decision|outcome|count),
	// so "the model now proposes an effect that gets denied" and "the model
	// stopped proposing it at all" do not read the same.
	Fields map[string]int `json:"fields"`
}

// RegressionReport is the whole run.
type RegressionReport struct {
	Sessions     int `json:"sessions"`
	Reproducible int `json:"reproducible"`
	Regressed    int `json:"regressed"`
	Failed       int `json:"failed"`
	// EffectsBefore and EffectsAfter total the sealed effects on each side.
	// A drop is the signal worth staring at: effects that stopped happening
	// are invisible in an output diff, because the absence of an action leaves
	// no text to compare.
	EffectsBefore int `json:"effects_before"`
	EffectsAfter  int `json:"effects_after"`

	ByCapability []CapabilityDelta `json:"by_capability,omitempty"`
	Results      []SessionResult   `json:"results"`

	// ModelsBefore and ModelsAfter are the distinct attestation hashes each
	// side cited. One entry on each side and they differ: this run compared
	// exactly two configurations, which is the case the command is for.
	ModelsBefore []string `json:"models_before,omitempty"`
	ModelsAfter  []string `json:"models_after,omitempty"`
}

// Divergent returns only the sessions whose behaviour changed — what a reader
// opens first, and the only part worth printing in full.
func (r RegressionReport) Divergent() []SessionResult {
	var out []SessionResult
	for _, s := range r.Results {
		if s.Regressed() {
			out = append(out, s)
		}
	}
	return out
}

// Clean reports whether nothing regressed. Sessions that failed to replay do
// not make a run dirty — they make it incomplete, which Failed says — because
// silently converting "could not test" into "test failed" trains people to
// ignore the result.
func (r RegressionReport) Clean() bool { return r.Regressed == 0 }

// Verdict is the one line a human or a CI job reads.
func (r RegressionReport) Verdict() string {
	switch {
	case r.Sessions == 0:
		return "no sessions with sealed effects to replay"
	case r.Regressed == 0 && r.Failed == 0:
		return fmt.Sprintf("%d/%d sessions reproduced every authorization decision",
			r.Reproducible, r.Sessions)
	case r.Regressed == 0:
		return fmt.Sprintf("%d/%d reproduced, %d could not be replayed",
			r.Reproducible, r.Sessions, r.Failed)
	default:
		return fmt.Sprintf("%d/%d sessions changed behaviour", r.Regressed, r.Sessions)
	}
}

// Regress folds per-session results into the run-level report.
func Regress(results []SessionResult) RegressionReport {
	r := RegressionReport{Sessions: len(results), Results: results}

	byCap := map[string]*CapabilityDelta{}
	modelsBefore, modelsAfter := map[string]bool{}, map[string]bool{}

	for _, s := range results {
		r.EffectsBefore += s.Diff.OldCount
		r.EffectsAfter += s.Diff.NewCount
		for _, m := range s.OldModels {
			modelsBefore[m] = true
		}
		for _, m := range s.NewModels {
			modelsAfter[m] = true
		}

		switch {
		case s.Error != "":
			r.Failed++
			continue
		case s.Diff.Reproducible():
			r.Reproducible++
			continue
		}
		r.Regressed++

		// A count divergence has no index to attribute, so it is charged to
		// the session rather than dropped — "the graph stopped sealing its
		// third effect" is the most consequential thing this can find and it
		// has no capability of its own to file under.
		seen := map[string]bool{}
		for _, d := range s.Diff.Divergences {
			cap := "(effect count)"
			if d.Field != "count" && d.Old != "" {
				cap = d.Old
				if d.Field != "capability" {
					cap = capabilityAt(s.Diff, d.Index)
				}
			}
			cd := byCap[cap]
			if cd == nil {
				cd = &CapabilityDelta{Capability: cap, Fields: map[string]int{}}
				byCap[cap] = cd
			}
			cd.Fields[d.Field]++
			if !seen[cap] {
				seen[cap] = true
				cd.Sessions++
			}
		}
	}

	for _, cd := range byCap {
		r.ByCapability = append(r.ByCapability, *cd)
	}
	sort.Slice(r.ByCapability, func(i, j int) bool {
		if r.ByCapability[i].Sessions != r.ByCapability[j].Sessions {
			return r.ByCapability[i].Sessions > r.ByCapability[j].Sessions
		}
		return r.ByCapability[i].Capability < r.ByCapability[j].Capability
	})
	r.ModelsBefore, r.ModelsAfter = sortedSet(modelsBefore), sortedSet(modelsAfter)
	return r
}

// capabilityAt recovers which capability a positional divergence belongs to.
// Divergence carries the values that differed, not the entry they came from,
// so for a decision or outcome change the capability has to come from a
// capability divergence at the same index — and when there is none, the two
// runs agreed on the capability and the label is simply unavailable here.
func capabilityAt(d DiffReport, index int) string {
	for _, other := range d.Divergences {
		if other.Index == index && other.Field == "capability" {
			return other.Old
		}
	}
	return fmt.Sprintf("(effect %d)", index+1)
}

// ModelsOf collects the distinct inference attestations cited across a set of
// entries — what argued for this run's effects, as a set.
//
// Nil rather than an empty slice when nothing was cited, matching
// normalizeInference: "no model argued for these effects" is a real statement
// about a webhook-driven or hand-driven run, and it should serialize as an
// absent field rather than an empty list that reads as a lookup failure.
func ModelsOf(entries []Entry) []string {
	seen := map[string]bool{}
	for _, e := range entries {
		for _, h := range e.Inference {
			seen[h] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return sortedSet(seen)
}
