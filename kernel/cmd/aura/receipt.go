package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/store"
)

// cmdReceipt implements `aura receipt` — build or check the portable evidence
// document for one sealed effect (C4 v1.2 / C5).
//
// Two modes, because evidence has two sides:
//
//	aura receipt <hash> [--out file.json]   issue: build it from this node
//	aura receipt --verify file.json         check: verify it as a third party
//
// The verify mode is the one that matters. It opens no database, contacts no
// node and needs no key material beyond what the document itself carries —
// so the party checking the evidence does not have to be, or trust, the party
// that produced it. That is the difference between an auditable system and
// one that merely logs.
func cmdReceipt(args []string) {
	fs := flag.NewFlagSet("receipt", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory to read from")
	out := fs.String("out", "", "write the receipt here instead of stdout")
	verify := fs.String("verify", "", "verify a receipt file instead of building one ('-' for stdin)")
	operands := parseWithOperands(fs, args, 1)

	if *verify != "" {
		if !runVerifyReceipt(os.Stdout, *verify) {
			os.Exit(1)
		}
		return
	}

	if len(operands) < 1 {
		fatal(fmt.Errorf("usage:\n" +
			"  aura receipt <effect-hash> [--out receipt.json]   build evidence for one effect\n" +
			"  aura receipt --verify receipt.json                check evidence, offline\n\n" +
			"  <effect-hash> is the receipt an effect's delivery returned — the same value\n" +
			"  `aura undo` accepts, and the `receipt` field on a sealed envelope."))
	}

	st, err := store.Open(*data)
	if err != nil {
		fatal(fmt.Errorf("open %s: %w", *data, err))
	}
	defer st.Close()

	r, err := ledger.BuildReceipt(st, operands[0])
	if err != nil {
		fatal(err)
	}
	blob, err := json.MarshalIndent(r, "", "  ")
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
	fmt.Printf("receipt written to %s (%d bytes)\n", abs, len(blob))
	fmt.Printf("  anchored to %d entries at %s\n", r.TreeSize, shortRoot(r.MerkleRoot))
	if len(r.Witnesses) > 0 {
		fmt.Printf("  %d witness countersignature(s)\n", len(r.Witnesses))
	} else {
		fmt.Println("  no witness countersignatures — run `aura witness <peer-url>` to anchor externally")
	}
	fmt.Println("\n  Anyone can now check it with:  aura receipt --verify " + *out)
}

// runVerifyReceipt checks a receipt file and prints a report. Separated from
// cmdReceipt so a test can drive it without deciding the process exit code.
func runVerifyReceipt(w io.Writer, path string) bool {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		fatal(fmt.Errorf("read receipt: %w", err))
	}

	var r ledger.Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		fatal(fmt.Errorf("%s is not a receipt document: %w", path, err))
	}
	rep := ledger.VerifyReceipt(r)

	var entry ledger.Entry
	_ = json.Unmarshal(r.Entry(), &entry)

	fmt.Fprintf(w, "aura receipt --verify %s\n\n", path)
	fmt.Fprintf(w, "  effect     %s\n", entry.Capability)
	fmt.Fprintf(w, "  actor      %s\n", entry.Actor)
	fmt.Fprintf(w, "  decision   %s → %s\n", entry.Decision, entry.Outcome)
	fmt.Fprintf(w, "  sealed by  %s (key %s)\n", r.Checkpoint.Node,
		ledger.Fingerprint(r.Checkpoint.Pubkey))
	fmt.Fprintf(w, "  position   entry %d of a tree of %d\n", entry.Seq, r.TreeSize)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s entry hash matches its content\n", tick(rep.EntryHashMatches))
	fmt.Fprintf(w, "  %s inclusion proof places it in the signed tree\n", tick(rep.InclusionValid))
	fmt.Fprintf(w, "  %s the node's signature over that tree head verifies\n", tick(rep.CheckpointValid))

	if rep.AttestationsCited > 0 {
		fmt.Fprintf(w, "  %s %d/%d cited inference attestation(s) resolved\n",
			tick(rep.AttestationsResolved == rep.AttestationsCited),
			rep.AttestationsResolved, rep.AttestationsCited)
		for hash := range r.Attestations {
			record, ok := r.Attestation(hash)
			if !ok {
				continue
			}
			var a ledger.Attestation
			if json.Unmarshal(record, &a) != nil {
				continue
			}
			fmt.Fprintf(w, "      %s  %s", a.Engine, a.Model)
			if a.Quantization != "" {
				fmt.Fprintf(w, " (%s)", a.Quantization)
			}
			if a.ModelRevision != "" {
				fmt.Fprintf(w, " @ %s", short(a.ModelRevision, 12))
			}
			fmt.Fprintf(w, "\n        %s\n", short(hash, 26))
		}
	}

	switch {
	case rep.Witnesses == 0:
		fmt.Fprintln(w, "  ·  no witnesses — this head rests on the sealing node's key alone")
	default:
		fmt.Fprintf(w, "  %s %d/%d witness countersignature(s) verify\n",
			tick(rep.WitnessesValid == rep.Witnesses), rep.WitnessesValid, rep.Witnesses)
	}

	if len(rep.Problems) > 0 {
		fmt.Fprintln(w, "\n  problems:")
		for _, p := range rep.Problems {
			fmt.Fprintf(w, "    → %s\n", p)
		}
	}

	if !rep.Sound() {
		fmt.Fprintln(w, "\n  NOT SOUND — this document is not evidence of the effect it describes.")
		return false
	}

	fmt.Fprintln(w, "\n  SOUND — this effect was sealed by that node, at that position, under that policy.")
	if rep.Witnesses == 0 {
		fmt.Fprintln(w, "\n  Scope: verified against the sealing node's own key. That key's holder could")
		fmt.Fprintln(w, "  have produced a different history and signed it just as validly. A witness")
		fmt.Fprintln(w, "  countersignature is what rules that out — see `aura witness`.")
	}
	if rep.AttestationsResolved > 0 {
		fmt.Fprintln(w, "\n  Scope: the inference attestations above are what the skill ASSERTED about")
		fmt.Fprintln(w, "  the model it ran. They are bound unforgeably to this effect and cannot be")
		fmt.Fprintln(w, "  backdated or detached — but nothing here proves the assertion was true.")
	}
	return true
}

func tick(ok bool) string {
	if ok {
		return "OK  "
	}
	return "FAIL"
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
