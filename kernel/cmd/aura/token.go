package main

// `aura token` — issue, list and revoke scoped credentials.
//
// A node's own bearer token is the operator's: it can do everything, and until
// this command existed it was the only credential there was, so every skill
// process ran with the operator's authority. See internal/gateway/scope.go for
// why that made two of the kernel's guarantees weaker than they read.
//
// The shape of the command follows `aura operator`, which solves the adjacent
// problem (who may approve) with the same structure — a roster, an issue verb, a
// revoke verb, and revocation recorded rather than deleted so that history stays
// explicable.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"aura/kernel/internal/gateway"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
)

// tokenResolver adapts the kernel store to gateway.TokenResolver.
//
// It lives here, in the composition root, rather than in either package: the
// gateway declares what it needs to authenticate a caller and must not depend on
// storage, while storage must not know which of its rows some HTTP layer treats
// as a credential. Wiring the two is this binary's job and nobody else's.
type tokenResolver struct{ st *store.Store }

func (t tokenResolver) TokenByHash(hash string) (gateway.TokenRow, bool, error) {
	row, found, err := t.st.TokenByHash(hash)
	if err != nil || !found {
		return gateway.TokenRow{}, false, err
	}
	return gateway.TokenRow{
		ID: row.ID, Scope: row.Scope, Capability: row.Capability,
		Label: row.Label, Revoked: row.Revoked,
	}, true, nil
}

func cmdToken(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: aura token issue --capability <cap> [--label <l>] | ls | revoke <id>"))
	}
	switch args[0] {
	case "issue":
		tokenIssue(args[1:])
	case "ls", "list":
		tokenList(args[1:])
	case "revoke", "rm":
		tokenRevoke(args[1:])
	default:
		fatal(fmt.Errorf("unknown token verb %q — want issue, ls or revoke", args[0]))
	}
}

// tokenIssue mints a credential bound to one capability.
//
// The token is printed once and never stored in the clear, which is the whole
// reason it can be printed at all: a node that could show it again would be a
// node whose database is a set of live credentials.
func tokenIssue(args []string) {
	fs := flag.NewFlagSet("token issue", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	capability := fs.String("capability", "", "the C1 capability this token may register as (required)")
	label := fs.String("label", "", "a human-readable note, e.g. which process holds it")
	_ = parseWithOperands(fs, args, 1)
	if *capability == "" && len(fs.Args()) > 0 {
		*capability = fs.Args()[0]
	}
	if strings.TrimSpace(*capability) == "" {
		fatal(fmt.Errorf("a skill token is bound to one capability: " +
			"aura token issue --capability motor.erp.write"))
	}
	// Validated against the same rule the manifest is, because a token bound to
	// a capability no manifest could declare is a token that can never register
	// — an error worth catching at issue rather than at 3am on a deploy.
	if err := registry.ValidateCapability(*capability); err != nil {
		fatal(fmt.Errorf("--capability: %w", err))
	}

	st, err := store.Open(*data)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	secret, err := newToken()
	if err != nil {
		fatal(err)
	}
	row := store.TokenRow{
		ID: "tok-" + shortID(), Scope: string(gateway.ScopeSkill),
		Capability: *capability, Label: *label, Created: time.Now().UnixMilli(),
	}
	if err := st.SaveToken(row, gateway.HashToken(secret)); err != nil {
		fatal(fmt.Errorf("store token: %w", err))
	}

	fmt.Printf("\n  %s  scoped to %s\n", row.ID, row.Capability)
	if row.Label != "" {
		fmt.Printf("  label     %s\n", row.Label)
	}
	fmt.Printf("\n  %s\n\n", secret)
	fmt.Printf("  This is the only time it is shown — only its hash is stored.\n")
	fmt.Printf("  Give it to the skill process as AURA_TOKEN, and it may:\n")
	fmt.Printf("    · connect to /ws/skill and register as %s, and nothing else\n", row.Capability)
	fmt.Printf("    · spend a receipt for the credentials that effect authorized\n")
	fmt.Printf("  It cannot register a graph, read the ledger, or enrol an operator.\n\n")
}

func tokenList(args []string) {
	fs := flag.NewFlagSet("token ls", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)

	st, err := store.Open(*data)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	rows, err := st.Tokens()
	if err != nil {
		fatal(err)
	}
	if *asJSON {
		out, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(out))
		return
	}
	if len(rows) == 0 {
		fmt.Println("no scoped tokens issued — every caller is using the operator token " +
			"(`aura token issue --capability <cap>` narrows one)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSCOPE\tCAPABILITY\tLABEL\tISSUED\tSTATE")
	for _, t := range rows {
		state := "live"
		if t.Revoked != 0 {
			state = "revoked " + time.UnixMilli(t.Revoked).Format("2006-01-02")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Scope, t.Capability, t.Label,
			time.UnixMilli(t.Created).Format("2006-01-02"), state)
	}
	w.Flush()
}

func tokenRevoke(args []string) {
	fs := flag.NewFlagSet("token revoke", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	_ = parseWithOperands(fs, args, 1)
	if len(fs.Args()) == 0 {
		fatal(fmt.Errorf("usage: aura token revoke <id>   (see `aura token ls`)"))
	}
	id := fs.Args()[0]

	st, err := store.Open(*data)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	ok, err := st.RevokeToken(id, time.Now().UnixMilli())
	if err != nil {
		fatal(err)
	}
	if !ok {
		fatal(fmt.Errorf("no live token %q — it may already be revoked (`aura token ls`)", id))
	}
	fmt.Printf("%s revoked. A process holding it is refused at its next request; "+
		"a connection it already has is not torn down.\n", id)
}

// newToken mints 256 bits of randomness, URL-safe so it survives a query string
// on the WebSocket path.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func shortID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
