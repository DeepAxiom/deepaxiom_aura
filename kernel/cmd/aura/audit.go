package main

// `aura audit` — the evidence an auditor asks for, over the period they asked
// about.
//
// The command is thin on purpose. Everything it prints comes from
// ledger.BuildAudit, and everything `--verify` checks comes from
// ledger.VerifyAudit, so the document and the terminal cannot disagree about
// what the evidence says. See internal/ledger/audit.go for why a period is the
// scope that was missing.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"aura/kernel/internal/identity"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/store"
)

func cmdAudit(args []string) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	since := fs.String("since", "", "start of the period (YYYY-MM-DD or RFC3339); default 90 days ago")
	until := fs.String("until", "", "end of the period, exclusive (YYYY-MM-DD or RFC3339); default now")
	out := fs.String("out", "", "write the report to this file instead of summarising it")
	verify := fs.String("verify", "", "verify a report written earlier, and exit")
	asJSON := fs.Bool("json", false, "emit the report as JSON on stdout")
	_ = fs.Parse(args)

	if *verify != "" {
		auditVerify(*verify)
		return
	}

	from, err := parseAuditTime(*since, time.Now().AddDate(0, 0, -90))
	if err != nil {
		fatal(fmt.Errorf("--since: %w", err))
	}
	to, err := parseAuditTime(*until, time.Now())
	if err != nil {
		fatal(fmt.Errorf("--until: %w", err))
	}

	st, err := store.Open(*data)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	// The node's own key, read from the data directory rather than from a
	// running node: this command has to work on a copied directory, months
	// later, on a machine where nothing is running. That is the same property
	// `aura verify` rests on.
	node, err := identity.Load(*data, identity.ModeLocal)
	if err != nil {
		fatal(err)
	}
	pub := node.Keys.PublicB64()

	// Force a checkpoint before building. Checkpoints are periodic, and an
	// effect sealed since the last one has no signed head for its inclusion
	// proof to point at — so a report generated minutes after a busy hour would
	// otherwise summarise effects it could attach no evidence for. Committing
	// the current head first is exactly what an auditor is asking the node to
	// do, and it is idempotent when nothing has changed.
	if ldg, err := ledger.Open(st, node.ID, node.Keys); err == nil {
		if err := ldg.Checkpoint(); err != nil {
			fmt.Fprintf(os.Stderr,
				"warning: could not checkpoint before auditing (%v); effects sealed since the "+
					"last checkpoint will be summarised without a receipt\n", err)
		}
	}

	rep, err := ledger.BuildAudit(st, node.ID, pub, from, to)
	if err != nil {
		fatal(err)
	}

	if *out != "" {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(*out, raw, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %s — %d effect(s) over %s to %s\n", *out, rep.Summary.Effects,
			time.UnixMilli(rep.From).Format("2006-01-02"),
			time.UnixMilli(rep.Until).Format("2006-01-02"))
		fmt.Printf("verify it anywhere with: aura audit --verify %s\n", *out)
		return
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(raw))
		return
	}
	printAudit(rep)
}

// parseAuditTime accepts a plain date, which is what anyone types, and RFC3339,
// which is what a script emits.
func parseAuditTime(s string, fallback time.Time) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return fallback, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither YYYY-MM-DD nor RFC3339", s)
	}
	return t, nil
}

func printAudit(rep ledger.AuditReport) {
	s := rep.Summary
	fmt.Printf("\n  aura audit — %s to %s\n  node %s\n\n",
		time.UnixMilli(rep.From).Format("2006-01-02"),
		time.UnixMilli(rep.Until).Format("2006-01-02"), rep.Node)

	if s.Effects == 0 {
		fmt.Printf("  Nothing acted on the world in this period.\n\n")
		printAuditIntegrity(rep.Integrity)
		return
	}

	fmt.Printf("  %d effect(s) acted on the world · %d delivered · %d refused\n",
		s.Effects, s.Delivered, s.Denied)
	fmt.Printf("  %d passed a human gate (%d signed, %d unsigned) · %d cleared by policy\n",
		s.Gated, s.Signed, s.UnsignedGated, s.Allowed)
	if s.Signed > 0 {
		// Reported beside the signed count rather than folded into it. "Somebody
		// answered this delivery" and "somebody consented to this document" are
		// different claims, and a reader who cannot see which one they have will
		// assume the stronger.
		fmt.Printf("  of the signed, %d also bind what the approver was shown\n", s.BoundApprovals)
	}
	fmt.Printf("  across %d session(s) and %d package version(s)\n", s.Sessions, s.DistinctActors)
	if s.Waived > 0 {
		fmt.Printf("  %d excused from their gate by the graph rather than by policy\n", s.Waived)
	}
	if s.Reversed > 0 {
		fmt.Printf("  %d were reversals of an earlier effect\n", s.Reversed)
	}

	fmt.Printf("\n  WHAT ACTED\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  CAPABILITY\tEFFECTS\tGATED\tREFUSED\tWAIVED")
	for _, c := range rep.Capabilities {
		fmt.Fprintf(w, "  %s\t%d\t%d\t%d\t%d\n", c.Capability, c.Effects, c.Gated, c.Denied, c.Waived)
	}
	w.Flush()

	if len(rep.Approvers) > 0 {
		fmt.Printf("\n  WHO SIGNED\n")
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  OPERATOR\tAPPROVED\tREFUSED")
		for _, a := range rep.Approvers {
			fmt.Fprintf(w, "  %s\t%d\t%d\n", a.Operator, a.Approved, a.Denied)
		}
		w.Flush()
	}

	fmt.Printf("\n  UNDER WHICH RULES\n")
	for _, p := range rep.Policies {
		fmt.Printf("  %s  %d effect(s), %s → %s\n", shortPolicy(p.Hash), p.Effects,
			time.UnixMilli(p.First).Format("2006-01-02"),
			time.UnixMilli(p.Last).Format("2006-01-02"))
	}

	fmt.Println()
	printAuditIntegrity(rep.Integrity)
	fmt.Printf("\n  `aura audit --out report.json` writes the same thing with a portable\n")
	fmt.Printf("  receipt per gated effect, verifiable by anyone with no node running.\n\n")
}

func printAuditIntegrity(in ledger.AuditIntegrity) {
	fmt.Printf("  INTEGRITY\n")
	fmt.Printf("  %d entries · chain %s · %d/%d checkpoint(s) valid · %d/%d witness signature(s)\n",
		in.Entries, boolWord(in.ChainIntact, "intact", "BROKEN"),
		in.CheckpointsValid, in.Checkpoints, in.WitnessesValid, in.Witnesses)
	if in.Sound {
		fmt.Printf("  SOUND — the record verifies from the database file alone.\n")
	} else {
		fmt.Printf("  NOT SOUND — %s\n", in.Detail)
	}
	if in.Witnesses == 0 {
		fmt.Printf("  Note: unwitnessed. This rests on the node's own key, which does not\n")
		fmt.Printf("  rule out the key's holder having rewritten it. `aura witness <peer>`.\n")
	}
}

func auditVerify(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	var rep ledger.AuditReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		fatal(fmt.Errorf("%s is not an audit report: %w", path, err))
	}
	v := ledger.VerifyAudit(rep)

	fmt.Printf("\n  %s — %s to %s, node %s\n", path,
		time.UnixMilli(rep.From).Format("2006-01-02"),
		time.UnixMilli(rep.Until).Format("2006-01-02"), rep.Node)
	fmt.Printf("  %d effect(s) · %d receipt(s) attached, %d verify\n",
		rep.Summary.Effects, v.ReceiptsChecked, v.ReceiptsValid)

	if n := len(rep.GatedOmitted); n > 0 {
		fmt.Printf("\n  %d gated effect(s) carry no receipt:\n", n)
		for i, o := range rep.GatedOmitted {
			if i == 5 {
				fmt.Printf("    ... and %d more\n", n-5)
				break
			}
			fmt.Printf("    %s — %s\n", shortPolicy(o.EntryHash), o.Reason)
		}
	}

	if len(v.Findings) > 0 {
		fmt.Println()
		for _, f := range v.Findings {
			marker := "  note "
			if f.Severity == "high" {
				marker = "  !!   "
			}
			fmt.Printf("%s%s\n", marker, f.Detail)
		}
	}

	fmt.Println()
	if v.Sound {
		fmt.Printf("  SOUND — every attached receipt verifies against the node key this\n")
		fmt.Printf("  report carries, and the counts match what is attached.\n\n")
		return
	}
	if v.Failure != "" {
		fmt.Printf("  NOT SOUND — %s\n\n", v.Failure)
	} else {
		fmt.Printf("  NOT SOUND — see the findings above.\n\n")
	}
	os.Exit(1)
}

func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func shortPolicy(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
