package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// tamperKernelDB opens a second, independent connection to the same
// kernel.db a *store.Store already wrote — standing in for "someone opened
// the file directly", the actual threat model `aura verify` exists to catch.
func tamperKernelDB(t *testing.T, dir, query string) {
	t.Helper()
	dsn := filepath.Join(dir, "kernel.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("tamper query: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	return os.WriteFile(path, []byte(content), 0o644)
}

// runVerify is the offline half of the ledger's trust story: it has to work
// against a plain data directory with no kernel involved, and it has to fail
// loudly — nonzero, not just a printed warning — the moment the chain or a
// checkpoint stops adding up.

func sealOneEffect(t *testing.T, dataDir string) *signing.Keypair {
	t.Helper()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	keys, err := signing.LoadOrCreate(filepath.Join(dataDir, "identity"))
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	ldg, err := ledger.Open(st, "node-test", keys)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	if _, err := ldg.Seal(ledger.SealRequest{
		Session: "sess-1", Envelope: "ENV-1", Cause: "ENV-0",
		Actor: "acme/motor/writer@1.0.0", Capability: "motor.erp.write",
		Decision: "allow", Outcome: "delivered",
		Policy: "sha256:policyhash", Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := ldg.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	return keys
}

func TestRunVerifyOnAFreshDataDirIsSoundAndEmpty(t *testing.T) {
	var out, errOut bytes.Buffer
	sound, err := runVerify(&out, &errOut, t.TempDir())
	if err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	if !sound {
		t.Fatal("an empty, untouched data dir reported unsound")
	}
	if !strings.Contains(out.String(), "0 entries") {
		t.Errorf("report does not mention an empty ledger: %s", out.String())
	}
}

// A data directory that was never started as a node has no identity key.
// That has to be a clearly-flagged degraded mode, not a fatal error — the
// hash chain is still checkable with no key at all.
func TestRunVerifyWithNoIdentityChecksTheChainOnly(t *testing.T) {
	dir := t.TempDir()
	// A ledger with no identity dir: append via the store directly so no
	// keypair gets created as a side effect of this test either.
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()

	var out, errOut bytes.Buffer
	sound, err := runVerify(&out, &errOut, dir)
	if err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	if !sound {
		t.Fatal("an empty ledger with no key reported unsound")
	}
	if !strings.Contains(errOut.String(), "no node identity found") {
		t.Errorf("no note about the missing identity: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "no key") {
		t.Errorf("the report does not say a key was unavailable: %s", out.String())
	}
}

func TestRunVerifyReportsSoundForARealEffect(t *testing.T) {
	dir := t.TempDir()
	sealOneEffect(t, dir)

	var out, errOut bytes.Buffer
	sound, err := runVerify(&out, &errOut, dir)
	if err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	if !sound {
		t.Fatalf("an untampered, checkpointed ledger reported unsound:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "1 entries") && !strings.Contains(out.String(), "SOUND") {
		t.Errorf("report does not confirm soundness: %s", out.String())
	}
}

// THE case this command exists for: a data directory whose ledger was
// tampered with must be reported unsound, with a nonzero-worthy `sound=false`
// — cmdVerify turns that into os.Exit(1), which is what a CI job checks.
func TestRunVerifyDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	sealOneEffect(t, dir)

	tamperKernelDB(t, dir, `UPDATE ledger_entries SET entry = REPLACE(entry, 'motor.erp.write', 'motor.payments.send') WHERE seq = 1`)

	var out, errOut bytes.Buffer
	sound, err := runVerify(&out, &errOut, dir)
	if err != nil {
		t.Fatalf("runVerify: %v", err)
	}
	if sound {
		t.Fatalf("a tampered ledger reported itself sound:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "NOT SOUND") {
		t.Errorf("report does not say NOT SOUND: %s", out.String())
	}
}

func TestRunVerifyOnAMissingDirectoryIsAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	// A path that cannot be created (a file standing where a directory is
	// expected) — store.Open must fail, and runVerify must report that as an
	// error rather than a false "sound".
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := writeFile(t, blocker, "not a directory"); err != nil {
		t.Fatal(err)
	}
	_, err := runVerify(&out, &errOut, filepath.Join(blocker, "nested"))
	if err == nil {
		t.Fatal("verifying an unusable path reported success")
	}
}
