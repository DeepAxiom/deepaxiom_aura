package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/store"
)

// cmdBundle implements `aura bundle` — the audit bundle for one session.
//
//	aura bundle <session> [--out case.json]   assemble it
//	aura bundle --verify case.json            check it, offline
//
// A receipt proves one effect; a bundle explains one session. It carries the
// trajectory, a standalone receipt per sealed effect, and the model
// configurations that argued for them — the four materials a 2026 result on
// agent evaluation (arXiv 2607.22368) identified as the difference between a
// score you can audit and a number you have to trust.
//
// Offline like `aura verify` and `aura receipt`: a bundle you can only produce
// while the node is up is a bundle you cannot produce during the incident that
// made you want one.
func cmdBundle(args []string) {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory to read from")
	out := fs.String("out", "", "write the bundle here instead of stdout")
	verify := fs.String("verify", "", "verify a bundle file instead of building one ('-' for stdin)")
	operands := parseWithOperands(fs, args, 1)

	if *verify != "" {
		if !runVerifyBundle(os.Stdout, *verify) {
			os.Exit(1)
		}
		return
	}

	if len(operands) < 1 {
		fatal(fmt.Errorf("usage:\n" +
			"  aura bundle <session> [--out case.json]   assemble one session's audit bundle\n" +
			"  aura bundle --verify case.json            check one, offline\n\n" +
			"  A bundle carries the session's trajectory, a verifiable receipt per sealed\n" +
			"  effect, and the model configurations behind them."))
	}

	st, err := store.Open(*data)
	if err != nil {
		fatal(fmt.Errorf("open %s: %w", *data, err))
	}
	defer st.Close()

	b, err := ledger.BuildBundle(st, operands[0])
	if err != nil {
		fatal(err)
	}
	blob, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		fatal(err)
	}

	if *out == "" {
		fmt.Println(string(blob))
		return
	}
	if err := os.WriteFile(*out, append(blob, '\n'), 0o644); err != nil {
		fatal(fmt.Errorf("write %s: %w", *out, err))
	}
	abs, _ := filepath.Abs(*out)
	fmt.Printf("audit bundle written to %s (%s)\n", abs, humanBytes(len(blob)))
	printBundleSummary(os.Stdout, b)
	fmt.Println("\n  Anyone can now check it with:  aura bundle --verify " + *out)
}

func printBundleSummary(w io.Writer, b ledger.Bundle) {
	s := b.Summary
	fmt.Fprintf(w, "  session    %s\n", b.Session)
	fmt.Fprintf(w, "  steps      %d", s.Steps)
	if b.TrajectoryTruncated {
		fmt.Fprintf(w, " (TRUNCATED at %d — this bundle is a prefix)", ledger.MaxTrajectorySteps)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  effects    %d sealed · %d delivered · %d denied\n",
		s.Effects, s.EffectsDelivered, s.EffectsDenied)
	if len(s.Models) > 0 {
		fmt.Fprintf(w, "  models     %v\n", s.Models)
	}
	if len(s.Capabilities) > 0 {
		fmt.Fprintf(w, "  acted on   %v\n", s.Capabilities)
	}
	if s.EnergyMJ > 0 {
		fmt.Fprintf(w, "  energy     %.1f mJ (%v)\n", s.EnergyMJ, s.EnergySources)
	}
	if s.StartedTS > 0 && s.EndedTS >= s.StartedTS {
		fmt.Fprintf(w, "  span       %s\n",
			time.Duration(s.EndedTS-s.StartedTS)*time.Millisecond)
	}
}

// runVerifyBundle checks a bundle file and prints a report.
func runVerifyBundle(w io.Writer, path string) bool {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		fatal(fmt.Errorf("read bundle: %w", err))
	}

	var b ledger.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		fatal(fmt.Errorf("%s is not an audit bundle: %w", path, err))
	}
	rep := ledger.VerifyBundle(b)

	fmt.Fprintf(w, "aura bundle --verify %s\n\n", path)
	printBundleSummary(w, b)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s trajectory hashes to what the bundle claims (%d steps)\n",
		tick(rep.TrajectoryIntact), rep.Steps)
	if rep.Effects > 0 {
		fmt.Fprintf(w, "  %s %d/%d sealed effect(s) verify standalone\n",
			tick(rep.EffectsSound == rep.Effects), rep.EffectsSound, rep.Effects)
	}
	if rep.Attestations > 0 {
		fmt.Fprintf(w, "  %s %d/%d model configuration(s) match their content address\n",
			tick(rep.AttestationsSound == rep.Attestations),
			rep.AttestationsSound, rep.Attestations)
	}

	if len(rep.Problems) > 0 {
		fmt.Fprintln(w, "\n  problems:")
		for _, p := range rep.Problems {
			fmt.Fprintf(w, "    → %s\n", p)
		}
	}

	if !rep.Sound() {
		fmt.Fprintln(w, "\n  NOT SOUND — this bundle does not support what it describes.")
		return false
	}

	fmt.Fprintln(w, "\n  SOUND — the trajectory, the effects and the model configurations")
	fmt.Fprintln(w, "  all check out against this document alone.")
	if rep.Truncated {
		fmt.Fprintln(w, "\n  Scope: the trajectory is a PREFIX. Everything present is verified;")
		fmt.Fprintln(w, "  the session continued past what this bundle carries.")
	}
	if rep.Attestations > 0 {
		fmt.Fprintln(w, "\n  Scope: model configurations are what each skill ASSERTED. They are")
		fmt.Fprintln(w, "  bound unforgeably to the effects they caused and cannot be backdated,")
		fmt.Fprintln(w, "  but nothing here proves an assertion was true.")
	}
	return true
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
