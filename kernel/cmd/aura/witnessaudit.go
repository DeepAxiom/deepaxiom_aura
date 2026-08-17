package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"os"
	"strings"
	"time"

	"aura/kernel/internal/ledger"
)

// `aura witness audit` — the monitor role, and the reason publishing a witness
// log is worth anything.
//
// Anchoring answers "can this node rewrite its own history?" — no, because a
// witness remembers. It leaves the next question open, and it is the one a
// sceptical reader asks immediately: **who watches the witness?**
//
// Certificate Transparency's answer is not "trust the log operator". It is that
// the logs publish their own Merkle history and independent monitors follow
// them, so a log that rewrites or forks is *caught*, publicly, by anyone who
// bothered to keep the previous head. That is this command. It is deliberately
// something any node can run against any witness, including the public one,
// including one you operate yourself.
//
//	aura witness audit                       # audit the public witness
//	aura witness audit http://peer:9080      # audit a specific one
//
// Three checks, and each is a distinct way a witness can be dishonest:
//
//  1. **The head is signed** by the key it claims. Otherwise you are following
//     a number somebody typed.
//  2. **The new head extends the one you already verified**, by RFC 6962
//     consistency proof. A witness that dropped or rewrote an entry it had
//     already published cannot produce this proof — there is nothing to forge,
//     the proof either exists because history really is an extension, or it
//     does not.
//  3. **The published tree really is the entries served**, rebuilt locally from
//     the log itself. A witness could otherwise sign a root unrelated to what
//     it hands out.
//
// What the monitor keeps between runs is the whole point: a snapshot proves
// nothing, a remembered snapshot proves a witness has not gone back on it.
func cmdWitnessAudit(args []string) {
	fs := flag.NewFlagSet("witness audit", flag.ExitOnError)
	port := fs.Int("port", 9080, "local node port (where the audit trail is kept)")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	token := fs.String("token", os.Getenv("AURA_WITNESS_TOKEN"),
		"bearer token, if the witness is not open")
	full := fs.Bool("full", false, "re-download and re-hash the whole log, not just the consistency proof")
	operands := parseWithOperands(fs, args, 1)

	remote := DefaultWitnessURL
	if len(operands) >= 1 {
		remote = strings.TrimRight(operands[0], "/")
	}
	c := &witnessClient{c: &nethttp.Client{Timeout: *timeout}, token: *token}
	local := newNodeClient(*port)

	fmt.Printf("\n  auditing witness %s\n\n", remote)

	// 1. The signed head.
	head, err := fetchWitnessHead(c, remote)
	if err != nil {
		fatal(err)
	}
	if err := ledger.VerifyLogHead(head); err != nil {
		fmt.Fprintf(os.Stderr, "  FAIL  the witness's head does not verify against its own key\n        %v\n\n", err)
		os.Exit(1)
	}
	fmt.Printf("  ok    head signed by %s\n", ledger.Fingerprint(head.WitnessKey))
	fmt.Printf("        %d countersignature(s) issued, root %s\n", head.Size, shortRoot(head.Root))

	// 2. Consistency against what this machine already verified.
	var seen struct {
		Head  ledger.LogHead `json:"head"`
		Known bool           `json:"known"`
	}
	if err := local.getJSON("/v1/witness/seen?key="+urlQueryEscape(head.WitnessKey), &seen); err != nil {
		// A node that cannot answer is not a reason to abandon the audit — the
		// signature and tree checks below still stand on their own.
		fmt.Printf("  --    no local record of this witness (%v)\n", err)
	}

	switch {
	case !seen.Known || seen.Head.Size == 0:
		// A recorded baseline of zero is the same position as no baseline at
		// all: every tree extends the empty one, so there is nothing this
		// witness could fail to prove yet. Saying "verified" here would be
		// claiming a check that did not happen.
		fmt.Printf("  --    nothing verified previously — recording %d as the baseline\n", head.Size)
	case seen.Head.Size > head.Size:
		fmt.Fprintf(os.Stderr,
			"\n  FAIL  this witness has SHRUNK: it published %d entries and now publishes %d.\n"+
				"        An append-only log does not lose entries. Either it was rolled back from\n"+
				"        a backup, or it is presenting a different history than it did before.\n\n",
			seen.Head.Size, head.Size)
		os.Exit(1)
	case seen.Head.Size == head.Size:
		if seen.Head.Root != head.Root {
			fmt.Fprintf(os.Stderr,
				"\n  FAIL  same size (%d), DIFFERENT root.\n"+
					"        was  %s\n        now  %s\n"+
					"        This witness rewrote history it had already published.\n\n",
				head.Size, seen.Head.Root, head.Root)
			os.Exit(1)
		}
		fmt.Printf("  ok    unchanged since the last audit (%d entries)\n", head.Size)
	default:
		if err := checkWitnessConsistency(c, remote, seen.Head, head); err != nil {
			fmt.Fprintf(os.Stderr,
				"\n  FAIL  this witness cannot prove its history still contains what it\n"+
					"        already published.\n        %v\n\n"+
					"        This is what a rewritten log looks like from the outside. The %d\n"+
					"        entries verified previously are the evidence; keep them.\n\n",
				err, seen.Head.Size)
			os.Exit(1)
		}
		fmt.Printf("  ok    grew %d → %d entries, and the old history is still a prefix\n",
			seen.Head.Size, head.Size)
	}

	// 3. Optionally rebuild the whole tree from the served entries.
	if *full && head.Size > 0 {
		if err := rebuildWitnessTree(c, remote, head); err != nil {
			fmt.Fprintf(os.Stderr, "\n  FAIL  %v\n\n", err)
			os.Exit(1)
		}
		fmt.Printf("  ok    the published root matches the entries it serves (%d rebuilt)\n", head.Size)
	}

	// 4. Remember, so the next audit has something to check against.
	body, _ := json.Marshal(map[string]any{"witness_url": remote, "head": head})
	if _, err := local.do("POST", "/v1/witness/seen", body); err != nil {
		fmt.Printf("  --    could not record this head locally (%v)\n", err)
	}

	fmt.Printf("\n  witness %s is accountable for %d countersignature(s).\n", remote, head.Size)
	fmt.Printf("  Run this again later: what it just committed to is what it will be held to.\n\n")
}

// cmdWitnessLog prints a witness's own log — what it has vouched for, for whom.
func cmdWitnessLog(args []string) {
	fs := flag.NewFlagSet("witness log", flag.ExitOnError)
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	token := fs.String("token", os.Getenv("AURA_WITNESS_TOKEN"), "bearer token, if not open")
	from := fs.Uint64("from", 1, "first entry")
	limit := fs.Int("limit", 50, "how many")
	operands := parseWithOperands(fs, args, 1)

	remote := DefaultWitnessURL
	if len(operands) >= 1 {
		remote = strings.TrimRight(operands[0], "/")
	}
	c := &witnessClient{c: &nethttp.Client{Timeout: *timeout}, token: *token}

	entries, err := fetchWitnessEntries(c, remote, *from, *limit)
	if err != nil {
		fatal(err)
	}
	if len(entries) == 0 {
		fmt.Printf("\n  %s has issued no countersignatures\n\n", remote)
		return
	}
	fmt.Printf("\n  %-6s %-24s %-8s %s\n", "SEQ", "NODE", "ENTRIES", "ROOT")
	for _, e := range entries {
		fmt.Printf("  %-6d %-24s %-8d %s\n", e.Seq, truncate(e.Node, 22), e.NodeSeq, shortRoot(e.MerkleRoot))
	}
	fmt.Println()
}

func fetchWitnessHead(c *witnessClient, remote string) (ledger.LogHead, error) {
	code, raw, err := c.do("GET", remote+"/v1/witness/head", nil)
	if err != nil {
		return ledger.LogHead{}, fmt.Errorf("reach witness %s: %w", remote, err)
	}
	if code == 404 {
		return ledger.LogHead{}, fmt.Errorf(
			"%s does not publish a witness log — it is either not a witness, or predates C4 v1.4. "+
				"An unpublished witness cannot be audited, which is the whole reason to prefer one that can", remote)
	}
	if code != 200 {
		return ledger.LogHead{}, fmt.Errorf("witness %s answered %d: %s",
			remote, code, strings.TrimSpace(string(raw)))
	}
	var head ledger.LogHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return ledger.LogHead{}, fmt.Errorf("unreadable head from %s: %w", remote, err)
	}
	return head, nil
}

func fetchWitnessEntries(c *witnessClient, remote string, from uint64, limit int) ([]ledger.WitnessRecord, error) {
	url := fmt.Sprintf("%s/v1/witness/log?from=%d&limit=%d", remote, from, limit)
	code, raw, err := c.do("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("reach witness %s: %w", remote, err)
	}
	if code != 200 {
		return nil, fmt.Errorf("witness %s answered %d: %s", remote, code, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Entries []ledger.WitnessRecord `json:"entries"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unreadable log from %s: %w", remote, err)
	}
	return out.Entries, nil
}

// checkWitnessConsistency asks the witness to prove its new head extends the
// one this machine already verified, and checks the proof locally.
//
// Checked here rather than trusted: a proof verified by the party that produced
// it is not a proof.
func checkWitnessConsistency(c *witnessClient, remote string, old, current ledger.LogHead) error {
	url := fmt.Sprintf("%s/v1/witness/consistency?from=%d&to=%d", remote, old.Size, current.Size)
	code, raw, err := c.do("GET", url, nil)
	if err != nil {
		return fmt.Errorf("reach witness: %w", err)
	}
	if code != 200 {
		return fmt.Errorf("the witness would not produce a consistency proof (%d): %s",
			code, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Proof []string `json:"proof"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("unreadable consistency proof: %w", err)
	}
	oldRoot, err := ledger.ParseHash(old.Root)
	if err != nil {
		return fmt.Errorf("the head recorded previously is unreadable: %w", err)
	}
	newRoot, err := ledger.ParseHash(current.Root)
	if err != nil {
		return fmt.Errorf("the head just served is unreadable: %w", err)
	}
	proof := make([]ledger.Hash, len(out.Proof))
	for i, s := range out.Proof {
		if proof[i], err = ledger.ParseHash(s); err != nil {
			return fmt.Errorf("proof element %d: %w", i, err)
		}
	}
	if !ledger.VerifyConsistency(int(old.Size), int(current.Size), oldRoot, newRoot, proof) {
		return fmt.Errorf("the consistency proof does not check out")
	}
	return nil
}

// rebuildWitnessTree re-downloads the whole log and recomputes the root.
//
// The consistency proof already catches a witness that rewrote published
// history. This catches a subtler one: a witness signing a root that has
// nothing to do with the entries it actually serves, so that the log a reader
// can inspect and the log it commits to are two different things.
func rebuildWitnessTree(c *witnessClient, remote string, head ledger.LogHead) error {
	var leaves []ledger.Hash
	for from := uint64(1); from <= head.Size; {
		entries, err := fetchWitnessEntries(c, remote, from, 1000)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return fmt.Errorf("the witness stopped serving entries at %d but claims %d", from, head.Size)
		}
		for _, e := range entries {
			raw, err := json.Marshal(e)
			if err != nil {
				return err
			}
			leaves = append(leaves, ledger.LeafHash(raw))
		}
		from += uint64(len(entries))
	}
	if uint64(len(leaves)) != head.Size {
		return fmt.Errorf("the witness claims %d entries and served %d", head.Size, len(leaves))
	}
	if got := ledger.MerkleRoot(leaves).String(); got != head.Root {
		return fmt.Errorf(
			"the root this witness SIGNED is not the root its own entries produce\n"+
				"        signed    %s\n        rebuilt   %s", head.Root, got)
	}
	return nil
}

// autoAnchor keeps this node's ledger anchored at a witness, in the background.
//
// The reason this is a flag on `aura up` and not a cron line in the docs: an
// anchor is only worth what somebody actually created, and every deployment
// that has to remember to run a command is a deployment where the anchor is
// months stale on the day it matters. Anchoring on a timer makes the guarantee
// a property of running the node rather than of operator discipline.
//
// Failures are logged and retried, never fatal. A witness being unreachable
// makes this node *unanchored*, which is the state it was in before — a weaker
// claim, not a broken node, and taking the node down over it would trade a
// safety property for an availability one.
func autoAnchor(target string, port int, every time.Duration, ldg *ledger.Ledger, log *slog.Logger) {
	remote := strings.TrimRight(target, "/")
	if remote == "default" || remote == "" {
		remote = DefaultWitnessURL
	}
	if every < time.Minute {
		every = time.Minute
	}
	log.Info("auto-anchoring enabled", "witness", remote, "every", every)

	// A short first delay so the listener is up: the anchoring path talks to
	// this node's own HTTP API, like the CLI does, rather than reaching into
	// the ledger directly. One code path for anchoring, whoever triggers it.
	time.Sleep(5 * time.Second)
	for {
		if err := anchorOnce(remote, port); err != nil {
			log.Warn("anchoring failed — this node is currently unanchored", "witness", remote, "err", err)
		} else {
			log.Info("ledger anchored", "witness", remote)
		}
		time.Sleep(every)
	}
}

// anchorOnce performs the handshake: ask what the witness last saw, present a
// head with a consistency proof from there, record what comes back.
func anchorOnce(remote string, port int) error {
	local := newNodeClient(port)
	c := &witnessClient{c: &nethttp.Client{Timeout: 30 * time.Second},
		token: os.Getenv("AURA_WITNESS_TOKEN")}

	var head ledger.Head
	if err := local.getJSON("/v1/ledger/head", &head); err != nil {
		return fmt.Errorf("read local head: %w", err)
	}
	if head.Seq == 0 {
		return nil // nothing sealed yet; not an error, just nothing to anchor
	}
	lastSeen, err := askLastSeen(c, remote, head.Node)
	if err != nil {
		return err
	}
	var stmt ledger.Statement
	if err := local.getJSON(fmt.Sprintf("/v1/ledger/statement?from_seq=%d", lastSeen), &stmt); err != nil {
		return fmt.Errorf("build statement: %w", err)
	}
	cs, err := requestCountersignature(c, remote, stmt)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"seq": stmt.Seq, "merkle_root": stmt.MerkleRoot,
		"witness_url": remote, "countersignature": cs,
	})
	if _, err := local.do("POST", "/v1/ledger/witness/record", body); err != nil {
		return fmt.Errorf("record countersignature: %w", err)
	}
	return nil
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '+':
			b.WriteString("%2B")
		case r == '/':
			b.WriteString("%2F")
		case r == '=':
			b.WriteString("%3D")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
