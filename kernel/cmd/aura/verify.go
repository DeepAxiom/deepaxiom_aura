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

	if r.Sound() {
		fmt.Fprintln(out, "\n  SOUND — the chain recomputes cleanly"+
			soundSuffix(r)+".")
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
