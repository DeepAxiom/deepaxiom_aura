package ledger

// The audit report — what acted on the world over a period, and under whose
// authority, in one verifiable document.
//
// # Why a period is the missing scope
//
// The ledger already answers two questions well. A **receipt** proves one
// effect. A **bundle** explains one session. Both are the shapes an engineer
// reaches for, because both start from something an engineer already has: an
// effect hash, a session id.
//
// An auditor has neither. They arrive with a date range and a question shaped
// like "show me everything this system did to the world in Q3, who authorized
// each one, on what basis, and demonstrate that the record has not been
// edited". Answering that from receipts means knowing which effects to ask
// about, which is the thing being audited. Answering it from the raw ledger
// means handing over the whole database — every unrelated effect, every other
// customer, permanently.
//
// So this is the third scope, and it is the one a compliance obligation
// actually names. The EU AI Act's Article 12 record-keeping duty, the
// `draft-sharif-agent-audit-trail` logging format and every internal control
// review ask for a period, not for a session.
//
// # What it carries, and what it deliberately does not
//
// The report is a summary plus evidence for the entries that need it. It
// carries:
//
//   - the verification of the chain, the checkpoints and any witness
//     countersignatures, so the record can be shown to be unedited;
//   - counts that answer the questions an auditor asks first — how many effects
//     acted, how many were gated, how many a human refused, how many a graph
//     excused rather than the policy;
//   - the distinct policy documents in force over the period, by hash, because
//     "under what rules" is the second question;
//   - a full portable receipt for every *gated* effect, which is the population
//     an auditor cares about: the ones a human was asked about.
//
// It does not carry a receipt for every allowed effect. A node sealing a
// million routine writes would produce a document nobody can open, and the
// summary plus the chain verification already establishes their integrity —
// any one of them can be exported individually with `aura receipt`. Where the
// cap bites, the report says so rather than appearing complete, the same rule
// the bundle's trajectory follows.
//
// Like the receipt and the bundle, it verifies with no database, no node and no
// network.

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
)

// AuditVersion identifies the document shape.
const AuditVersion = "aura-audit-report/1"

// MaxAuditReceipts bounds how many full receipts a report embeds.
//
// Gated effects are rare by construction — each one interrupted a human — so
// this is generous in practice and exists for the pathological case rather than
// the ordinary one.
const MaxAuditReceipts = 1000

// AuditReport is the document.
type AuditReport struct {
	Version string `json:"version"`
	Node    string `json:"node"`
	// NodePubkey is what every signature in this document verifies against, so
	// a reader needs nothing from the node that produced it.
	NodePubkey string `json:"node_pubkey"`
	Generated  int64  `json:"generated"`

	// From and Until bound the period, in unix millis. Until is exclusive, so
	// consecutive reports tile without double-counting an effect on a boundary.
	From  int64 `json:"from"`
	Until int64 `json:"until"`

	Summary AuditSummary `json:"summary"`
	// Policies are the distinct policy documents in force over the period, with
	// how many effects each authorized. More than one is normal — a policy
	// changed mid-period — and is exactly what an auditor wants flagged rather
	// than averaged away.
	Policies []AuditPolicy `json:"policies"`
	// Capabilities breaks the period down by what actually acted.
	Capabilities []AuditCapability `json:"capabilities"`
	// Inference reports the distinct model configurations cited over the period
	// and how much of what they claim rests on the skill's word.
	Inference AuditInference `json:"inference"`
	// Approvers counts sealed approvals per operator. Present only for effects
	// whose gate was answered with a verified signature.
	Approvers []AuditApprover `json:"approvers,omitempty"`
	// Gated carries a full portable receipt per gated effect, in sequence order.
	Gated []Receipt `json:"gated,omitempty"`
	// GatedTruncated is how many gated effects had to be left out of Gated
	// because of MaxAuditReceipts. Non-zero means this document is a sample,
	// and it says so.
	GatedTruncated int `json:"gated_truncated,omitempty"`
	// GatedOmitted names every gated effect whose receipt could not be built,
	// with the reason.
	//
	// Named rather than silently dropped. The overwhelmingly common cause is
	// benign — an effect sealed since the last periodic checkpoint has no signed
	// head to prove inclusion against yet — but "the evidence for this one is
	// missing, and here is why" is a thing an auditor must be told rather than
	// left to infer from a count that does not add up.
	GatedOmitted []AuditOmission `json:"gated_omitted,omitempty"`

	// Integrity is the whole-ledger verification at generation time. It covers
	// the chain beyond this period deliberately: an edit anywhere breaks every
	// hash after it, so a period cannot be shown intact in isolation.
	Integrity AuditIntegrity `json:"integrity"`
}

// AuditSummary is the top of the document, and the part most readers stop at.
type AuditSummary struct {
	Effects   int `json:"effects"`
	Delivered int `json:"delivered"`
	Denied    int `json:"denied"`
	// Gated is how many effects a human was asked about; Allowed is how many
	// the policy cleared without asking.
	Gated   int `json:"gated"`
	Allowed int `json:"allowed"`
	// Signed is how many gated effects carry a verified operator signature.
	// Unsigned gated effects are the gap a reviewer should look at: somebody
	// answered, and the node's own word is the only evidence of who.
	Signed         int `json:"signed_approvals"`
	UnsignedGated  int `json:"unsigned_gated"`
	Waived         int `json:"waived"`
	Reversed       int `json:"reversed"`
	WithInference  int `json:"with_inference"`
	Sessions       int `json:"sessions"`
	DistinctActors int `json:"distinct_actors"`
}

// AuditOmission is one piece of evidence the report could not attach.
type AuditOmission struct {
	EntryHash string `json:"entry_hash"`
	Reason    string `json:"reason"`
}

// AuditInference is what argued for the period's effects, and how far each
// claim can be checked.
//
// The counts are the answer to a question an auditor asks and a summary of
// effect counts cannot address: *how much of this do I have to take the
// operator”'s word for?* A model configuration a skill merely declared and one
// a TEE quote binds to that exact declaration are different kinds of evidence,
// and reporting them as one number would launder the weaker into the stronger —
// the same mistake C5 spends a page warning about with energy figures.
type AuditInference struct {
	// Cited is how many distinct attestations the period'''s effects referenced.
	Cited int `json:"cited"`
	// SelfDeclared, Bound and Verified partition Cited by EvidenceLevel.
	SelfDeclared int `json:"self_declared"`
	Bound        int `json:"bound"`
	Verified     int `json:"verified"`
	// Unreadable counts attestations the ledger cites that could not be loaded
	// or no longer match their content address. Non-zero is a finding.
	Unreadable int `json:"unreadable,omitempty"`
	// Models lists the distinct model identifiers cited, sorted.
	Models []string `json:"models,omitempty"`
}

// AuditPolicy is one policy document that was in force.
type AuditPolicy struct {
	Hash    string `json:"hash"`
	Effects int    `json:"effects"`
	First   int64  `json:"first"`
	Last    int64  `json:"last"`
}

// AuditCapability is one capability's activity over the period.
type AuditCapability struct {
	Capability string `json:"capability"`
	Effects    int    `json:"effects"`
	Gated      int    `json:"gated"`
	Denied     int    `json:"denied"`
	Waived     int    `json:"waived"`
}

// AuditApprover is one human's approval count.
type AuditApprover struct {
	Operator string `json:"operator"`
	Approved int    `json:"approved"`
	Denied   int    `json:"denied"`
}

// AuditIntegrity is the ledger verification carried inside the report.
type AuditIntegrity struct {
	Entries          int64  `json:"entries"`
	ChainIntact      bool   `json:"chain_intact"`
	Checkpoints      int    `json:"checkpoints"`
	CheckpointsValid int    `json:"checkpoints_valid"`
	Witnesses        int    `json:"witnesses"`
	WitnessesValid   int    `json:"witnesses_valid"`
	ApprovalsInvalid int    `json:"approvals_invalid"`
	Sound            bool   `json:"sound"`
	Detail           string `json:"detail,omitempty"`
}

// BuildAudit assembles the report for [from, until).
func BuildAudit(st *store.Store, nodeID, pubkeyB64 string, from, until time.Time) (AuditReport, error) {
	return BuildAuditWith(st, nodeID, pubkeyB64, from, until, TrustAnchors{})
}

// BuildAuditWith is BuildAudit with the operator”'s TEE trust anchors, which
// decide whether hardware evidence can reach EvidenceVerified. Separate rather
// than a sixth positional argument so the common call stays readable.
func BuildAuditWith(st *store.Store, nodeID, pubkeyB64 string, from, until time.Time,
	anchors TrustAnchors) (AuditReport, error) {
	if !until.After(from) {
		return AuditReport{}, fmt.Errorf("the period ends at or before it starts (%s → %s)",
			from.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	fromMS, untilMS := from.UnixMilli(), until.UnixMilli()

	raw, err := st.LedgerEntries(1, 0)
	if err != nil {
		return AuditReport{}, fmt.Errorf("read ledger: %w", err)
	}

	rep := AuditReport{
		Version: AuditVersion, Node: nodeID, NodePubkey: pubkeyB64,
		Generated: time.Now().UnixMilli(), From: fromMS, Until: untilMS,
	}

	policies := map[string]*AuditPolicy{}
	caps := map[string]*AuditCapability{}
	approvers := map[string]*AuditApprover{}
	sessions := map[string]bool{}
	actors := map[string]bool{}
	cited := map[string]bool{}
	var gatedHashes []string

	for _, r := range raw {
		var e Entry
		if err := json.Unmarshal(r, &e); err != nil {
			// An entry that does not parse is a fact about the ledger, not a
			// reason to abandon the report. The integrity section below is what
			// reports it properly.
			continue
		}
		if e.TS < fromMS || e.TS >= untilMS {
			continue
		}

		s := &rep.Summary
		s.Effects++
		sessions[e.Session] = true
		actors[e.Actor] = true

		switch e.Outcome {
		case spec.OutcomeDelivered:
			s.Delivered++
		case spec.OutcomeDenied:
			s.Denied++
		}
		if e.Decision == spec.DecisionGate {
			s.Gated++
			gatedHashes = append(gatedHashes, e.Hash())
			if e.Approver != nil {
				s.Signed++
			} else {
				s.UnsignedGated++
			}
		} else {
			s.Allowed++
		}
		if e.Waived {
			s.Waived++
		}
		if e.Compensates != "" {
			s.Reversed++
		}
		if len(e.Inference) > 0 {
			s.WithInference++
		}
		for _, h := range e.Inference {
			cited[h] = true
		}

		p := policies[e.Policy]
		if p == nil {
			p = &AuditPolicy{Hash: e.Policy, First: e.TS, Last: e.TS}
			policies[e.Policy] = p
		}
		p.Effects++
		if e.TS < p.First {
			p.First = e.TS
		}
		if e.TS > p.Last {
			p.Last = e.TS
		}

		c := caps[e.Capability]
		if c == nil {
			c = &AuditCapability{Capability: e.Capability}
			caps[e.Capability] = c
		}
		c.Effects++
		if e.Decision == spec.DecisionGate {
			c.Gated++
		}
		if e.Outcome == spec.OutcomeDenied {
			c.Denied++
		}
		if e.Waived {
			c.Waived++
		}

		if e.Approver != nil {
			a := approvers[e.Approver.Operator]
			if a == nil {
				a = &AuditApprover{Operator: e.Approver.Operator}
				approvers[e.Approver.Operator] = a
			}
			if e.Approver.Decision == ApprovalApprove {
				a.Approved++
			} else {
				a.Denied++
			}
		}
	}

	rep.Summary.Sessions = len(sessions)
	rep.Summary.DistinctActors = len(actors)

	// Sorted output throughout, so two reports over the same period are
	// byte-identical. A document whose ordering depends on map iteration cannot
	// be diffed, and diffing consecutive periods is most of what a reviewer does.
	for _, p := range policies {
		rep.Policies = append(rep.Policies, *p)
	}
	sort.Slice(rep.Policies, func(i, j int) bool { return rep.Policies[i].First < rep.Policies[j].First })
	for _, c := range caps {
		rep.Capabilities = append(rep.Capabilities, *c)
	}
	sort.Slice(rep.Capabilities, func(i, j int) bool {
		if rep.Capabilities[i].Effects != rep.Capabilities[j].Effects {
			return rep.Capabilities[i].Effects > rep.Capabilities[j].Effects
		}
		return rep.Capabilities[i].Capability < rep.Capabilities[j].Capability
	})
	for _, a := range approvers {
		rep.Approvers = append(rep.Approvers, *a)
	}
	sort.Slice(rep.Approvers, func(i, j int) bool { return rep.Approvers[i].Operator < rep.Approvers[j].Operator })

	// A full receipt per gated effect: the population an auditor is actually
	// reviewing, each independently verifiable without the rest of the ledger.
	if len(gatedHashes) > MaxAuditReceipts {
		rep.GatedTruncated = len(gatedHashes) - MaxAuditReceipts
		gatedHashes = gatedHashes[:MaxAuditReceipts]
	}
	for _, h := range gatedHashes {
		rc, err := BuildReceipt(st, h)
		if err != nil {
			// One unbuildable receipt must not cost the report — but it must be
			// named. The usual cause is an effect sealed since the last periodic
			// checkpoint, which `aura audit` forces before building precisely so
			// this stays rare.
			rep.GatedOmitted = append(rep.GatedOmitted, AuditOmission{
				EntryHash: h, Reason: err.Error(),
			})
			continue
		}
		rep.Gated = append(rep.Gated, rc)
	}

	// What argued for these effects, and how far each claim can be checked.
	// Loaded from storage rather than trusted from the entry, because the point
	// of the citation is that the record it names is still there and still
	// hashes to what was cited.
	models := map[string]bool{}
	for _, h := range sortedSet(cited) {
		raw, err := st.Attestation(h)
		if err != nil || AttestationHash(raw) != h {
			rep.Inference.Unreadable++
			continue
		}
		rep.Inference.Cited++
		if a, _, err := ParseAttestation(raw); err == nil && a.Model != "" {
			models[a.Model] = true
		}
		switch CheckEvidence(raw, anchors).Level {
		case EvidenceVerified:
			rep.Inference.Verified++
		case EvidenceBound:
			rep.Inference.Bound++
		default:
			rep.Inference.SelfDeclared++
		}
	}
	rep.Inference.Models = sortedSet(models)

	// The integrity section covers the whole chain, not the period. An edit
	// anywhere rewrites every hash after it, so a period cannot be shown intact
	// on its own — and a report that implied otherwise would be the most
	// dangerous kind of wrong.
	vr, err := Verify(st, pubkeyB64)
	if err != nil {
		return rep, fmt.Errorf("verify ledger: %w", err)
	}
	rep.Integrity = AuditIntegrity{
		Entries: vr.TotalEntries, ChainIntact: vr.ChainIntact,
		Checkpoints: vr.Checkpoints, CheckpointsValid: vr.CheckpointsValid,
		Witnesses: vr.Witnesses, WitnessesValid: vr.WitnessesValid,
		ApprovalsInvalid: vr.ApprovalsInvalid,
		Sound:            vr.Sound(), Detail: vr.ApprovalFailure,
	}
	return rep, nil
}

// AuditFinding is one thing a reviewer should look at, in a report that
// verified. Findings are not failures — they are the questions the evidence
// raises.
type AuditFinding struct {
	Severity string `json:"severity"` // "high" | "note"
	Detail   string `json:"detail"`
}

// AuditVerdict is what VerifyAudit concluded.
type AuditVerdict struct {
	Sound bool `json:"sound"`
	// ReceiptsChecked and ReceiptsValid cover the embedded receipts, each
	// re-verified from the document rather than trusted.
	ReceiptsChecked int            `json:"receipts_checked"`
	ReceiptsValid   int            `json:"receipts_valid"`
	Findings        []AuditFinding `json:"findings,omitempty"`
	Failure         string         `json:"failure,omitempty"`
}

// VerifyAudit re-checks a report from the document alone: every embedded
// receipt is verified against the node key the document carries, the counts are
// checked against the receipts present, and the integrity section is read for
// what it says.
//
// It opens no database and contacts no node, which is the property that makes
// the report worth sending to somebody.
func VerifyAudit(rep AuditReport) AuditVerdict {
	var v AuditVerdict

	if rep.Version != AuditVersion {
		v.Failure = fmt.Sprintf("unknown document version %q (this binary reads %q)",
			rep.Version, AuditVersion)
		return v
	}
	if rep.NodePubkey == "" {
		v.Failure = "the report carries no node key, so nothing in it can be checked"
		return v
	}
	if rep.Until <= rep.From {
		v.Failure = "the report's period ends at or before it starts"
		return v
	}

	for _, rc := range rep.Gated {
		v.ReceiptsChecked++
		// Bound each receipt to the node the report claims to be about: a
		// report could otherwise be padded with genuine receipts from somebody
		// else's ledger, each of which verifies perfectly on its own.
		if rc.Checkpoint.Pubkey != rep.NodePubkey {
			v.Findings = append(v.Findings, AuditFinding{
				Severity: "high",
				Detail: fmt.Sprintf("effect %s was sealed by a different node than this report is about",
					shortHash(rc.EntryHash)),
			})
			continue
		}
		if VerifyReceipt(rc).Sound() {
			v.ReceiptsValid++
			continue
		}
		v.Findings = append(v.Findings, AuditFinding{
			Severity: "high",
			Detail:   fmt.Sprintf("the receipt for effect %s does not verify", shortHash(rc.EntryHash)),
		})
	}

	// The counts must be consistent with what is embedded, or the summary is
	// describing a different document than the one attached.
	if want := rep.Summary.Gated - rep.GatedTruncated - len(rep.GatedOmitted); want != len(rep.Gated) {
		v.Findings = append(v.Findings, AuditFinding{
			Severity: "high",
			Detail: fmt.Sprintf("the summary counts %d gated effects, %d truncated and %d omitted, "+
				"but %d receipts are attached", rep.Summary.Gated, rep.GatedTruncated,
				len(rep.GatedOmitted), len(rep.Gated)),
		})
	}
	if rep.Summary.Gated+rep.Summary.Allowed != rep.Summary.Effects {
		v.Findings = append(v.Findings, AuditFinding{Severity: "high",
			Detail: "the gated and allowed counts do not add up to the effect count"})
	}
	if rep.Summary.Delivered+rep.Summary.Denied != rep.Summary.Effects {
		v.Findings = append(v.Findings, AuditFinding{Severity: "high",
			Detail: "the delivered and denied counts do not add up to the effect count"})
	}

	// Notes: things that are not wrong, and that a reviewer is being paid to
	// notice.
	if rep.Summary.UnsignedGated > 0 {
		v.Findings = append(v.Findings, AuditFinding{Severity: "note",
			Detail: fmt.Sprintf("%d gated effect(s) carry no operator signature — a human answered, "+
				"and the node's own word is the only record of who. Set `require_signed_approval` "+
				"in policy to make this impossible", rep.Summary.UnsignedGated)})
	}
	if rep.Summary.Waived > 0 {
		v.Findings = append(v.Findings, AuditFinding{Severity: "note",
			Detail: fmt.Sprintf("%d effect(s) were excused from their gate by the graph rather than "+
				"by policy — see `waived` in C4", rep.Summary.Waived)})
	}
	if n := rep.Inference.Unreadable; n > 0 {
		v.Findings = append(v.Findings, AuditFinding{Severity: "high",
			Detail: fmt.Sprintf("%d cited inference attestation(s) could not be read or no longer "+
				"match their content address — the ledger points at evidence that is gone or "+
				"altered", n)})
	}
	if rep.Inference.Cited > 0 && rep.Inference.Bound+rep.Inference.Verified == 0 {
		v.Findings = append(v.Findings, AuditFinding{Severity: "note",
			Detail: fmt.Sprintf("all %d model configuration(s) cited are self-declared: the skill "+
				"stated what it ran and no hardware evidence binds the claim to the run. "+
				"Verifiable: who claimed what, when, and what it caused. Not verifiable: "+
				"whether the claim was true", rep.Inference.Cited)})
	}
	if rep.Inference.Bound > 0 && rep.Inference.Verified == 0 {
		v.Findings = append(v.Findings, AuditFinding{Severity: "note",
			Detail: fmt.Sprintf("%d model configuration(s) carry hardware evidence bound to the "+
				"declaration, but no vendor trust anchor was configured, so the hardware'''s own "+
				"signature was not checked", rep.Inference.Bound)})
	}
	if rep.Integrity.Witnesses == 0 {
		v.Findings = append(v.Findings, AuditFinding{Severity: "note",
			Detail: "no third party has counter-signed this history, so it rests on the node's own " +
				"key — which does not rule out the key's holder having rewritten it"})
	}
	if n := len(rep.GatedOmitted); n > 0 {
		v.Findings = append(v.Findings, AuditFinding{Severity: "note",
			Detail: fmt.Sprintf("%d gated effect(s) are counted but carry no receipt; the report "+
				"names each one and why (usually: sealed after the last signed checkpoint)", n)})
	}
	if rep.GatedTruncated > 0 {
		v.Findings = append(v.Findings, AuditFinding{Severity: "note",
			Detail: fmt.Sprintf("%d gated effect(s) are summarised but not attached; export them "+
				"individually with `aura receipt`", rep.GatedTruncated)})
	}

	high := 0
	for _, f := range v.Findings {
		if f.Severity == "high" {
			high++
		}
	}
	// The integrity section is the node's own claim about itself, so a report
	// that says it is unsound is unsound — but a report that says it is sound
	// is only as good as the receipts, which is why both are required.
	v.Sound = high == 0 &&
		rep.Integrity.Sound &&
		v.ReceiptsValid == v.ReceiptsChecked
	if !rep.Integrity.Sound {
		v.Failure = "the ledger this report was generated from did not verify: " + rep.Integrity.Detail
	}
	return v
}
