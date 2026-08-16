package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"aura/kernel/internal/ledger"
)

// cmdRegress — replay many recorded sessions and report what the change did to
// the *effects*, using the ledger as the oracle.
//
//	aura regress [--sessions 50] [--graph <id>] [--json] [--fail-on-regression]
//
// The question it exists for is the one that arrives with every model upgrade
// and that nothing else answers with evidence: we are about to change the
// model — what does that do to what this system actually *does* to the world?
//
// An eval suite scores outputs against a rubric and cannot see that a refund
// stopped being issued. Observability records the aftermath. Neither can say
// "339 of 345 sessions sealed exactly the same effects; here are the 6 that did
// not, and here is the model revision on each side." This can, because C5 binds
// the model that argued for an act to the act itself, and C4 sealed both runs.
//
// Deliberately not a pass/fail gate by default. A model-backed graph legitimately
// words things differently run to run, and a tool that cries regression at every
// rephrasing gets muted within a week — which is how it stops being read at the
// one moment it matters. `--fail-on-regression` is there for a CI job that has
// decided its graphs are deterministic enough to hold to it.
func cmdRegress(args []string) {
	fs := flag.NewFlagSet("regress", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	limit := fs.Int("sessions", 25, "how many recent sessions to replay")
	graphFilter := fs.String("graph", "", "only sessions of this graph")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	failOn := fs.Bool("fail-on-regression", false, "exit non-zero if any session changed behaviour")
	denyGates := fs.Bool("deny-gates", false, "deny gates during replay instead of approving")
	_ = fs.Parse(args)

	sessions := recentSessionsWithEffects(*port, *limit, *graphFilter)
	if len(sessions) == 0 {
		fmt.Print("\n  no sessions with sealed effects to replay — nothing to compare\n\n")
		return
	}

	fmt.Printf("\n  replaying %d session(s) against the current graphs and policy…\n\n", len(sessions))

	results := make([]ledger.SessionResult, 0, len(sessions))
	for i, s := range sessions {
		fmt.Printf("  [%d/%d] %s ", i+1, len(sessions), s)
		res := replayOne(*port, s, *graphFilter, *denyGates)
		switch {
		case res.Error != "":
			fmt.Printf("· could not replay: %s\n", res.Error)
		case res.Diff.Reproducible():
			fmt.Printf("· reproduced (%d effect(s))\n", res.Diff.NewCount)
		default:
			fmt.Printf("· CHANGED (%d divergence(s))\n", len(res.Diff.Divergences))
		}
		results = append(results, res)
	}

	report := ledger.Regress(results)

	if *asJSON {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
	} else {
		printRegressionReport(*port, report)
	}

	if *failOn && !report.Clean() {
		os.Exit(1)
	}
}

func printRegressionReport(port int, r ledger.RegressionReport) {
	fmt.Printf("\n  ── regression report ──────────────────────────────────\n\n")
	fmt.Printf("  sessions      %d replayed · %d reproduced · %d changed · %d could not run\n",
		r.Sessions, r.Reproducible, r.Regressed, r.Failed)
	fmt.Printf("  effects       %d sealed before · %d after", r.EffectsBefore, r.EffectsAfter)
	if r.EffectsAfter < r.EffectsBefore {
		// Called out because it is the finding an output diff structurally
		// cannot make: an act that stopped happening leaves no text behind to
		// compare against, so it reads as silence rather than as a change.
		fmt.Printf("   ← %d fewer effects happened", r.EffectsBefore-r.EffectsAfter)
	}
	fmt.Println()

	if len(r.ModelsBefore) > 0 || len(r.ModelsAfter) > 0 {
		fmt.Printf("\n  models cited\n")
		printModelSide(port, "before", r.ModelsBefore)
		printModelSide(port, "after ", r.ModelsAfter)
	}

	if len(r.ByCapability) > 0 {
		fmt.Printf("\n  what changed, by capability\n")
		for _, c := range r.ByCapability {
			fields := make([]string, 0, len(c.Fields))
			for f, n := range c.Fields {
				fields = append(fields, fmt.Sprintf("%s×%d", f, n))
			}
			sort.Strings(fields)
			fmt.Printf("    %-40s %d session(s)  [%s]\n",
				c.Capability, c.Sessions, strings.Join(fields, " "))
		}
	}

	regressed := r.Divergent()
	if len(regressed) > 0 {
		fmt.Printf("\n  sessions that changed\n")
		for _, s := range regressed {
			fmt.Printf("    %s → replay %s\n", s.Session, s.Replay)
			for _, d := range s.Diff.Divergences {
				if d.Field == "count" {
					fmt.Printf("        effect count %s → %s\n", d.Old, d.New)
					continue
				}
				fmt.Printf("        effect %d %s: %q → %q\n", d.Index+1, d.Field, d.Old, d.New)
			}
			fmt.Printf("        aura why %s\n", s.Replay)
		}
	}

	fmt.Printf("\n  verdict: %s\n\n", r.Verdict())
}

// printModelSide resolves attestation hashes to something a human can act on.
// A hash tells you two runs differed; "Qwen2.5-1.5B @f1d2d2f (Q4_K_M)" tells
// you which upgrade to roll back.
func printModelSide(port int, label string, hashes []string) {
	if len(hashes) == 0 {
		fmt.Printf("    %s  (none cited)\n", label)
		return
	}
	for _, h := range hashes {
		fmt.Printf("    %s  %s\n", label, describeAttestation(port, h))
	}
}

func describeAttestation(port int, hash string) string {
	code, raw := getRaw(port, "/v1/ledger/attestations/"+hash)
	if code != 200 {
		return shortHash(hash)
	}
	var a ledger.Attestation
	if json.Unmarshal(raw, &a) != nil {
		return shortHash(hash)
	}
	s := a.Model
	if s == "" {
		s = a.Engine
	}
	if a.ModelRevision != "" {
		s += " @" + shortRevision(a.ModelRevision)
	}
	if a.Quantization != "" {
		s += " (" + a.Quantization + ")"
	}
	return s
}

func shortRevision(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func shortHash(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// recentSessionsWithEffects picks what to replay.
//
// Sessions that sealed no effects are skipped rather than replayed and
// reported as trivially reproducible: they would inflate the pass count with
// conversations that could not have regressed, which is exactly the kind of
// reassuring-but-empty number that makes a report stop being read.
func recentSessionsWithEffects(port, limit int, graphFilter string) []string {
	code, raw := getRaw(port, "/v1/sessions")
	if code != 200 {
		fatal(fmt.Errorf("cannot list sessions — is a node running on port %d?", port))
	}
	var out struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			GraphID   string `json:"graph_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		fatal(err)
	}

	var picked []string
	for _, s := range out.Sessions {
		if len(picked) >= limit {
			break
		}
		if graphFilter != "" && s.GraphID != graphFilter {
			continue
		}
		// A replay session is itself a session, and replaying replays would
		// compound drift and count the same original twice.
		if strings.HasPrefix(s.SessionID, "sess-replay") {
			continue
		}
		if len(fetchLedgerBySession(port, s.SessionID)) == 0 {
			continue
		}
		picked = append(picked, s.SessionID)
	}
	return picked
}
