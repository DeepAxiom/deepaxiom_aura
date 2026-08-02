package ledger

import "strconv"

// Deterministic replay against the ledger as oracle — the fourth property of
// the project's thesis (ROADMAP.md): Authorized, Attested, Reversible,
// Reproducible. `aura replay` already re-runs a session's recorded client
// inputs against the current graph and diffs what the client would have
// seen; that proves the conversation looked the same, not that the
// *authorization* did. Diff is the other half: given the entries the
// original run sealed and the entries replaying the same inputs sealed, does
// the ledger — not the transcript — agree they behaved the same?
//
// This file is deliberately pure: no store, no HTTP, nothing that needs a
// running kernel to exercise. `aura replay` is the only caller, and it
// supplies both entry slices already fetched from `GET
// /v1/sessions/{id}/ledger`.

// Divergence is a position where replay disagrees with the original run on
// something that defines "did the same authorization happen": which
// capability was invoked, what the policy decided, or what became of it.
// Any Divergence means DiffReport.Reproducible() is false.
type Divergence struct {
	Index int    `json:"index"`
	Field string `json:"field"` // "capability" | "decision" | "outcome" | "count"
	Old   string `json:"old"`
	New   string `json:"new"`
}

// Note is a position where replay disagrees with the original run on
// something that legitimately varies run to run — a skill's version, the
// policy document's hash, the payload an effect actually carried, or
// whether a compensation was declared. None of these make replay
// unreproducible by themselves (a model-backed skill wording a write
// differently is not an authorization failure — see replay.go's own
// rationale for why the transcript diff already tolerates this), but for a
// `motor` effect "did the same thing actually happen" is exactly what an
// auditor reading a replay report needs to see, not have silently dropped.
type Note struct {
	Index int    `json:"index"`
	Field string `json:"field"` // "actor" | "policy" | "payload_sha256" | "compensation"
	Old   string `json:"old"`
	New   string `json:"new"`
}

// DiffReport is what comparing two ordered slices of sealed entries
// produces. Named distinctly from verify.go's Report (the chain-integrity
// report `aura verify` produces) — same package, two different questions:
// one asks "is this ledger's own history intact", the other asks "did
// replaying these inputs reproduce what it recorded".
type DiffReport struct {
	OldCount    int          `json:"old_count"`
	NewCount    int          `json:"new_count"`
	Compared    int          `json:"compared"`
	Divergences []Divergence `json:"divergences,omitempty"`
	Notes       []Note       `json:"notes,omitempty"`
}

// Reproducible reports whether replay reproduced every authorization
// decision the original run made. Notes do not affect it.
func (r DiffReport) Reproducible() bool { return len(r.Divergences) == 0 }

// Diff compares old (what the original session sealed) against new (what
// replaying its inputs against the current graph and policy sealed),
// positionally. Position is the only order comparison means anything under:
// both slices come from GET /v1/sessions/{id}/ledger, already in seq order,
// so entry 3 has to line up with entry 3 — matching by content would hide
// exactly the kind of divergence (a capability invoked in a different order,
// or not at all) this exists to catch.
func Diff(old, new []Entry) DiffReport {
	r := DiffReport{OldCount: len(old), NewCount: len(new)}

	if len(old) != len(new) {
		r.Divergences = append(r.Divergences, Divergence{
			Field: "count",
			Old:   strconv.Itoa(len(old)),
			New:   strconv.Itoa(len(new)),
		})
	}

	n := len(old)
	if len(new) < n {
		n = len(new)
	}
	r.Compared = n

	for i := 0; i < n; i++ {
		o, w := old[i], new[i]

		diverge := func(field, oldV, newV string) {
			r.Divergences = append(r.Divergences, Divergence{Index: i, Field: field, Old: oldV, New: newV})
		}
		note := func(field, oldV, newV string) {
			r.Notes = append(r.Notes, Note{Index: i, Field: field, Old: oldV, New: newV})
		}

		if o.Capability != w.Capability {
			diverge("capability", o.Capability, w.Capability)
		}
		if o.Decision != w.Decision {
			diverge("decision", o.Decision, w.Decision)
		}
		if o.Outcome != w.Outcome {
			diverge("outcome", o.Outcome, w.Outcome)
		}

		if o.Actor != w.Actor {
			note("actor", o.Actor, w.Actor)
		}
		if o.Policy != w.Policy {
			note("policy", o.Policy, w.Policy)
		}
		if o.PayloadSHA256 != w.PayloadSHA256 {
			note("payload_sha256", o.PayloadSHA256, w.PayloadSHA256)
		}
		if compensationShape(o.Compensation) != compensationShape(w.Compensation) {
			note("compensation", compensationShape(o.Compensation), compensationShape(w.Compensation))
		}
	}
	return r
}

// compensationShape renders a Compensation (or its absence) as a comparable
// string — "" means irreversible, matching how the field itself is omitted
// from the JSON entry rather than null-valued.
func compensationShape(c *Compensation) string {
	if c == nil {
		return ""
	}
	return c.Capability + " -> " + c.Port
}
