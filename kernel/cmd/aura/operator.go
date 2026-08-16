package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aura/kernel/internal/approvals"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// `aura operator` — who may answer a gate, and the keypair that proves it.
//
// The private key is deliberately not kept in the node's data directory. A node
// that held it could sign approvals on its operators' behalf, which would make
// every sealed approval worthless: the entire value of C4 v1.3 is that the
// party being audited cannot manufacture the evidence. So keys live under the
// operator's own home directory and only the public half is ever sent.
//
// On a single-machine install the same person owns both directories and the
// separation is a convention rather than a boundary — worth saying plainly
// rather than implying more than is true. It becomes real the moment the
// operator approves from their own laptop against a node somewhere else, which
// is the deployment this is shaped for.

// operatorKeyDir is where an operator's own keypair lives.
func operatorKeyDir(id string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".aura", "operator", id)
}

func cmdOperator(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: aura operator <enroll|add|list|revoke|whoami> [...]\n\n" +
			"  enroll <id>            generate a keypair locally and enrol its public half\n" +
			"  add <id> --pubkey <b64>  enrol a key generated somewhere else\n" +
			"  list                   who may approve on this node\n" +
			"  revoke <id>            stop them approving (past approvals stay valid)\n" +
			"  whoami <id>            print this operator's public key"))
	}
	switch args[0] {
	case "enroll":
		cmdOperatorEnroll(args[1:])
	case "add":
		cmdOperatorAdd(args[1:])
	case "list":
		cmdOperatorList(args[1:])
	case "revoke":
		cmdOperatorRevoke(args[1:])
	case "whoami":
		cmdOperatorWhoami(args[1:])
	default:
		fatal(fmt.Errorf("unknown operator subcommand %q", args[0]))
	}
}

func cmdOperatorEnroll(args []string) {
	fs := flag.NewFlagSet("operator enroll", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	name := fs.String("name", "", "human-readable name")
	rest := parseWithOperands(fs, args, 1)
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura operator enroll <id> [--name \"Grace Hopper\"]"))
	}
	id := rest[0]

	dir := operatorKeyDir(id)
	keys, err := signing.LoadOrCreate(dir)
	if err != nil {
		fatal(fmt.Errorf("create operator keypair: %w", err))
	}
	pub := keys.PublicB64()

	body, _ := json.Marshal(map[string]string{"id": id, "pubkey": pub, "name": *name})
	if _, err := newNodeClient(*port).do("POST", "/v1/operators", body); err != nil {
		fatal(err)
	}
	fmt.Printf("\n  enrolled %s\n", id)
	fmt.Printf("  public key   %s\n", pub)
	fmt.Printf("  private key  %s\n", filepath.Join(dir, "ed25519.key"))
	fmt.Printf("\n  This node now accepts approvals signed by that key and by no other.\n")
	fmt.Printf("  Approve with:  aura approve <id> --as %s\n\n", id)
}

func cmdOperatorAdd(args []string) {
	fs := flag.NewFlagSet("operator add", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	pubkey := fs.String("pubkey", "", "base64 ed25519 public key")
	name := fs.String("name", "", "human-readable name")
	rest := parseWithOperands(fs, args, 1)
	if len(rest) != 1 || *pubkey == "" {
		fatal(fmt.Errorf("usage: aura operator add <id> --pubkey <base64>"))
	}
	body, _ := json.Marshal(map[string]string{"id": rest[0], "pubkey": *pubkey, "name": *name})
	if _, err := newNodeClient(*port).do("POST", "/v1/operators", body); err != nil {
		fatal(err)
	}
	fmt.Printf("  enrolled %s\n", rest[0])
}

func cmdOperatorList(args []string) {
	fs := flag.NewFlagSet("operator list", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	_ = fs.Parse(args)

	code, raw := getRaw(*port, "/v1/operators")
	if code != 200 {
		fatal(fmt.Errorf("this node has no operator roster"))
	}
	var out struct {
		Operators []store.OperatorRow `json:"operators"`
	}
	_ = json.Unmarshal(raw, &out)
	if len(out.Operators) == 0 {
		fmt.Print("\n  no operators enrolled — every gate is answered unsigned\n" +
			"  enrol one:  aura operator enroll <id>\n\n")
		return
	}
	fmt.Printf("\n  %-20s %-46s %s\n", "OPERATOR", "PUBLIC KEY", "STATUS")
	for _, o := range out.Operators {
		status := "active"
		if o.Revoked != 0 {
			status = "revoked " + time.UnixMilli(o.Revoked).Format("2006-01-02")
		}
		fmt.Printf("  %-20s %-46s %s\n", o.ID, truncate(o.Pubkey, 44), status)
	}
	fmt.Println()
}

func cmdOperatorRevoke(args []string) {
	fs := flag.NewFlagSet("operator revoke", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	rest := parseWithOperands(fs, args, 1)
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura operator revoke <id>"))
	}
	if _, err := newNodeClient(*port).do("DELETE", "/v1/operators/"+rest[0], nil); err != nil {
		fatal(err)
	}
	fmt.Printf("\n  revoked %s — they can approve nothing further.\n", rest[0])
	fmt.Printf("  Approvals they already gave remain valid and still verify; that is\n")
	fmt.Printf("  deliberate, since an audit trail that changes when the roster does\n")
	fmt.Printf("  is not one.\n\n")
}

func cmdOperatorWhoami(args []string) {
	fs := flag.NewFlagSet("operator whoami", flag.ExitOnError)
	rest := parseWithOperands(fs, args, 1)
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura operator whoami <id>"))
	}
	pub, err := signing.LoadPublicKey(operatorKeyDir(rest[0]))
	if err != nil {
		fatal(fmt.Errorf("no local keypair for %q — run `aura operator enroll %s`", rest[0], rest[0]))
	}
	fmt.Printf("  %s  %s\n", rest[0], pub)
}

// signApprovalFor builds the signed statement for one pending approval.
//
// The values signed over come from the node's own listing rather than from
// flags, and that is a safety property, not a convenience: an operator cannot
// be tricked into signing a statement about a delivery other than the one they
// were shown, because the only delivery they can sign is the one the node says
// it is holding.
func signApprovalFor(port int, approvalID, operator string, approve bool) (*ledger.Approval, error) {
	code, raw := getRaw(port, "/v1/approvals")
	if code != 200 {
		return nil, fmt.Errorf("this node has no approval queue to sign against")
	}
	var out struct {
		Approvals []approvals.Pending `json:"approvals"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	var p *approvals.Pending
	for i := range out.Approvals {
		if out.Approvals[i].ID == approvalID {
			p = &out.Approvals[i]
			break
		}
	}
	if p == nil {
		return nil, fmt.Errorf("no pending approval %q", approvalID)
	}
	if p.Node == "" || p.Envelope == "" {
		return nil, fmt.Errorf("approval %q does not name the node and delivery to sign over — "+
			"it was raised by a surface that predates signed approval", approvalID)
	}

	keys, err := signing.LoadOrCreate(operatorKeyDir(operator))
	if err != nil {
		return nil, fmt.Errorf("load keypair for %q: %w", operator, err)
	}
	decision := ledger.ApprovalApprove
	if !approve {
		decision = ledger.ApprovalDeny
	}
	return ledger.SignApproval(operator, keys.Private,
		p.Node, p.Session, p.Envelope, decision, time.Now().UnixMilli())
}

// ── secrets ─────────────────────────────────────────────────────────

func cmdSecret(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: aura secret <set|ls|rm> [...]\n\n" +
			"  set <name> [value]   store a credential (reads stdin if value is omitted)\n" +
			"  ls                   list configured secret names — never values\n" +
			"  rm <name>            forget one\n\n" +
			"A stored secret is released only against the receipt of an effect that\n" +
			"passed the Effect Checkpoint. Reference it as ${secret:<name>}."))
	}
	switch args[0] {
	case "set":
		cmdSecretSet(args[1:])
	case "ls", "list":
		cmdSecretList(args[1:])
	case "rm", "delete":
		cmdSecretRemove(args[1:])
	default:
		fatal(fmt.Errorf("unknown secret subcommand %q", args[0]))
	}
}

func cmdSecretSet(args []string) {
	fs := flag.NewFlagSet("secret set", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	rest := parseWithOperands(fs, args, 2)
	if len(rest) == 0 {
		fatal(fmt.Errorf("usage: aura secret set <name> [value]   (value omitted = read stdin)"))
	}
	name := rest[0]
	var value string
	if len(rest) > 1 {
		value = rest[1]
	} else {
		// Reading from stdin keeps the credential out of the shell history and
		// out of the process table, which an argument cannot be.
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil || n == 0 {
				break
			}
		}
		value = string(buf)
		for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
			value = value[:len(value)-1]
		}
	}
	if value == "" {
		fatal(fmt.Errorf("refusing to store an empty value for %q", name))
	}
	body, _ := json.Marshal(map[string]string{"name": name, "value": value})
	if _, err := newNodeClient(*port).do("PUT", "/v1/secrets", body); err != nil {
		fatal(err)
	}
	fmt.Printf("\n  stored %s — reference it as ${secret:%s}\n", name, name)
	fmt.Printf("  It is encrypted with this node's identity key and is never served back.\n\n")
}

func cmdSecretList(args []string) {
	fs := flag.NewFlagSet("secret ls", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	_ = fs.Parse(args)
	code, raw := getRaw(*port, "/v1/secrets")
	if code != 200 {
		fatal(fmt.Errorf("this node has no credential broker"))
	}
	var out struct {
		Secrets []string `json:"secrets"`
	}
	_ = json.Unmarshal(raw, &out)
	if len(out.Secrets) == 0 {
		fmt.Print("\n  no secrets configured\n\n")
		return
	}
	fmt.Println()
	for _, n := range out.Secrets {
		fmt.Printf("  ${secret:%s}\n", n)
	}
	fmt.Println()
}

func cmdSecretRemove(args []string) {
	fs := flag.NewFlagSet("secret rm", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	rest := parseWithOperands(fs, args, 1)
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura secret rm <name>"))
	}
	if _, err := newNodeClient(*port).do("DELETE", "/v1/secrets/"+rest[0], nil); err != nil {
		fatal(err)
	}
	fmt.Printf("  removed %s\n", rest[0])
}
