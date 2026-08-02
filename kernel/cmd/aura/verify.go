package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// cmdVerify implements `aura verify` — offline recomputation of the effect
// ledger's hash chain and checkpoint signatures.
//
// "Offline" is the load-bearing word: this opens the SQLite file directly and
// never talks to a running kernel, over HTTP or otherwise. That is what turns
// the ledger from a log a node's operator asserts is trustworthy into evidence
// anyone holding the data directory and the node's public key can check for
// themselves — an auditor, a court, a successor operator who does not trust
// whoever ran the node. `GET /v1/ledger/verify` runs the identical function
// (kernel/internal/ledger.Verify) against a live node for convenience; this
// command is the one that works when there is no node left to ask.
//
// It never touches the node's private key. Checking a signature only ever
// needs the public half — see signing.LoadPublicKey, which errors rather than
// creating anything if no identity exists yet, because a verification tool
// that minted a keypair as a side effect would be lying about what it verified.
func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory to verify")
	_ = fs.Parse(args)

	sound, err := runVerify(os.Stdout, os.Stderr, *data)
	if err != nil {
		fatal(err)
	}
	if !sound {
		os.Exit(1)
	}
}

// runVerify holds everything cmdVerify does except deciding the process exit
// code, so it can be exercised directly by a test: open the data directory,
// load whatever public key is there (none is not fatal — see below), run the
// chain-and-signature check, and print the human-readable report.
func runVerify(out, errOut io.Writer, dataDir string) (sound bool, err error) {
	st, err := store.Open(dataDir)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", dataDir, err)
	}
	defer st.Close()

	pubkey, keyErr := signing.LoadPublicKey(filepath.Join(dataDir, "identity"))
	if keyErr != nil {
		fmt.Fprintf(errOut,
			"note: no node identity found at %s — checking the hash chain only, "+
				"not checkpoint signatures (%v)\n\n", dataDir, keyErr)
	}

	report, err := ledger.Verify(st, pubkey)
	if err != nil {
		return false, fmt.Errorf("verify: %w", err)
	}

	printVerifyReport(out, dataDir, report)
	return report.Sound(), nil
}

func printVerifyReport(out io.Writer, dataDir string, r ledger.Report) {
	chainWord := "chain intact"
	if !r.ChainIntact {
		chainWord = fmt.Sprintf("CHAIN BROKEN at seq %d: %s", r.BrokenAtSeq, r.BrokenReason)
	}

	checkpointWord := "no checkpoints"
	switch {
	case !r.KeyAvailable && r.Checkpoints > 0:
		checkpointWord = fmt.Sprintf("%d checkpoint(s) present, signatures NOT checked (no public key)", r.Checkpoints)
	case r.Checkpoints > 0:
		checkpointWord = fmt.Sprintf("%d/%d checkpoint(s) valid", r.CheckpointsValid, r.Checkpoints)
	}

	keyWord := "no key"
	if r.KeyAvailable {
		keyWord = "key " + r.PubkeyFingerprint
	}

	fmt.Fprintf(out, "aura verify — %s\n\n", dataDir)
	fmt.Fprintf(out, "  %d entries · %s · %s · %s\n", r.TotalEntries, chainWord, checkpointWord, keyWord)
	if r.MerkleRoot != "" {
		fmt.Fprintf(out, "  merkle head %s", shortRoot(r.MerkleRoot))
		if r.MerkleCheckpoints > 0 {
			fmt.Fprintf(out, " · %d checkpoint(s) commit to a tree head", r.MerkleCheckpoints)
		}
		fmt.Fprintln(out)
	}
	if r.Witnesses > 0 {
		fmt.Fprintf(out, "  %d/%d witness countersignature(s) verify\n", r.WitnessesValid, r.Witnesses)
	}

	if r.Sound() {
		fmt.Fprintln(out, "\n  SOUND — the chain recomputes cleanly"+
			soundSuffix(r)+".")
		printAnchoringScope(out, r)
		return
	}
	fmt.Fprintln(out, "\n  NOT SOUND.")
	if !r.ChainIntact {
		fmt.Fprintf(out, "  → an entry at or before seq %d does not match what came after it — "+
			"content was altered, deleted, or reordered after sealing.\n", r.BrokenAtSeq)
	}
	if r.KeyAvailable && r.CheckpointsValid != r.Checkpoints {
		fmt.Fprintf(out, "  → %d of %d checkpoints do not verify — either a signature was forged, "+
			"or the chain was rewritten and made internally consistent again without "+
			"the node's private key.\n", r.Checkpoints-r.CheckpointsValid, r.Checkpoints)
	}
	if r.MerkleMismatches > 0 {
		fmt.Fprintf(out, "  → %d checkpoint(s) carry a validly-signed tree head that these entries "+
			"do not produce. The node signed a history different from the one stored here.\n",
			r.MerkleMismatches)
	}
	if r.Witnesses > r.WitnessesValid {
		fmt.Fprintf(out, "  → %d of %d witness countersignatures no longer match. A third party "+
			"signed a head this ledger no longer produces.\n",
			r.Witnesses-r.WitnessesValid, r.Witnesses)
		if r.ChainIntact && r.CheckpointsValid == r.Checkpoints {
			// Everything self-referential passes and only the outside
			// disagrees — the signature of a rewrite performed by whoever
			// holds this node's key. Say so, because a reader looking at
			// "chain intact · checkpoints valid" will otherwise conclude the
			// opposite of what happened.
			fmt.Fprintln(out,
				"     Note: the chain and every checkpoint verify. That combination —\n"+
					"     internally perfect, externally contradicted — is what a rewrite by the\n"+
					"     holder of this node's own key looks like. Ask the witness directly.")
		}
	}
}

// printAnchoringScope states what a passing verification does and does not
// establish.
//
// A bare "SOUND" over-reads: everything checked so far was checked against
// the sealing node's own key, so it rules out an attacker without that key
// and does not rule out the key's holder. Saying so on every clean run —
// rather than in documentation nobody reads at the moment it matters — is the
// difference between an honest tool and a reassuring one.
func printAnchoringScope(out io.Writer, r ledger.Report) {
	if r.WitnessesValid > 0 {
		fmt.Fprintf(out, "\n  Anchored: %d third-party countersignature(s) over this history. A rewrite\n"+
			"  would have to make every witness forget what it already signed.\n", r.WitnessesValid)
		return
	}
	fmt.Fprintln(out, "\n  Scope: verified against this node's own key. That establishes nobody")
	fmt.Fprintln(out, "  altered the ledger WITHOUT the key — not that the key's holder didn't.")
	fmt.Fprintln(out, "  Run `aura witness <peer-url>` to anchor this history with a third party.")
}

func soundSuffix(r ledger.Report) string {
	if r.KeyAvailable && r.Checkpoints > 0 {
		return " and every checkpoint signature verifies"
	}
	if !r.KeyAvailable {
		return " (checkpoint signatures were not checked — no public key given)"
	}
	return ""
}
