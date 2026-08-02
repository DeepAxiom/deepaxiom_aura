package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/store"
)

// cmdBOM implements `aura bom` — a CycloneDX ML-BOM built from what a session
// actually ran, not from what it was configured to run.
//
//	aura bom                      every sealed effect on this node
//	aura bom sess-8f3a            one session
//	aura bom --out inventory.json
//
// Offline, like `aura verify`: it reads the data directory and never asks a
// running node. An inventory you can only produce while the system is up is
// not much use during the incident that made you want one.
func cmdBOM(args []string) {
	fs := flag.NewFlagSet("bom", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory to read from")
	out := fs.String("out", "", "write the BOM here instead of stdout")
	operands := parseWithOperands(fs, args, 1)

	session := ""
	if len(operands) > 0 {
		session = operands[0]
	}

	st, err := store.Open(*data)
	if err != nil {
		fatal(fmt.Errorf("open %s: %w", *data, err))
	}
	defer st.Close()

	bom, err := ledger.BuildBOM(st, session, version)
	if err != nil {
		fatal(err)
	}
	blob, err := json.MarshalIndent(bom, "", "  ")
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

	models, skills := 0, 0
	for _, c := range bom.Components {
		if c.Type == "machine-learning-model" {
			models++
		} else {
			skills++
		}
	}
	abs, _ := filepath.Abs(*out)
	fmt.Printf("CycloneDX %s ML-BOM written to %s\n", bom.SpecVersion, abs)
	fmt.Printf("  scope   %s\n", bomScopeLabel(session))
	fmt.Printf("  %d skill component(s), %d model component(s)\n", skills, models)
	if models == 0 {
		fmt.Println("\n  No models appear here. Either no inference contributed to these effects,")
		fmt.Println("  or the skills involved do not attest (C5) — see `aura receipt` for one")
		fmt.Println("  effect's evidence, and the SDK's `attest=` argument for how a skill declares.")
	}
}

func bomScopeLabel(session string) string {
	if session == "" {
		return "this node, every sealed effect"
	}
	return "session " + session
}
