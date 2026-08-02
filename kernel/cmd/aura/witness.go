package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	nethttp "net/http"
	"os"
	"strings"
	"time"

	"aura/kernel/internal/ledger"
)

// witnessClient talks to a *remote* node — a different machine with a
// different bearer token, which is why it cannot reuse nodeClient.
type witnessClient struct {
	c     *nethttp.Client
	token string
}

func (w *witnessClient) do(method, url string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := nethttp.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := w.c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// cmdWitness implements `aura witness` — external anchoring of this node's
// effect ledger (C4 v1.2).
//
// The gap it closes is the one self-signing cannot. A node signs its own head
// with its own key, so an operator holding that key can rewrite history and
// re-sign the result: the chain recomputes, every signature verifies, and
// nothing in the data directory records that yesterday it said something
// else. A witness is a second party that remembers what it was already shown
// and refuses to vouch for a history that contradicts it.
//
// One round trip:
//
//	aura witness http://peer:9080
//	  → ask the peer what it last saw of us
//	  → build a statement: our signed head + an RFC 6962 consistency proof
//	    from that point
//	  → the peer verifies both and counter-signs, or refuses
//	  → we record the countersignature, and `aura verify` reports it
//
// The peer needs no special software: every node is a witness (see main.go),
// so any two nodes can anchor each other.
func cmdWitness(args []string) {
	fs := flag.NewFlagSet("witness", flag.ExitOnError)
	port := fs.Int("port", 9080, "local node port")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	// The witness is a different node with a different bearer token, so the
	// local one cannot be reused. `/v1/ledger/witness` is behind auth like
	// every other /v1 route — see the note at the bottom of this file on why
	// that is the conservative choice and what it costs.
	remoteToken := fs.String("token", os.Getenv("AURA_WITNESS_TOKEN"),
		"bearer token for the witness node (or AURA_WITNESS_TOKEN)")
	operands := parseWithOperands(fs, args, 1)

	if len(operands) < 1 {
		fatal(fmt.Errorf("usage: aura witness <witness-url> [--token <t>] [--port 9080]\n\n" +
			"  <witness-url> is another node that will vouch for this one's ledger,\n" +
			"  e.g. http://peer.internal:9080 — any node can act as a witness.\n\n" +
			"  --token is the WITNESS node's bearer token (or AURA_WITNESS_TOKEN).\n" +
			"  Not needed if that node runs with --no-auth."))
	}
	remote := strings.TrimRight(operands[0], "/")
	local := newNodeClient(*port)
	http := &witnessClient{c: &nethttp.Client{Timeout: *timeout}, token: *remoteToken}

	// 1. Who are we, and what has this witness already seen of us?
	var head ledger.Head
	if err := local.getJSON("/v1/ledger/head", &head); err != nil {
		fatal(fmt.Errorf("read local ledger head: %w", err))
	}
	if head.Seq == 0 {
		fatal(fmt.Errorf("this node has sealed no effects yet — there is nothing to witness"))
	}

	lastSeen, err := askLastSeen(http, remote, head.Node)
	if err != nil {
		fatal(err)
	}

	// 2. Build the statement, which forces a checkpoint if the current head
	//    is not yet committed under our own key.
	var stmt ledger.Statement
	path := fmt.Sprintf("/v1/ledger/statement?from_seq=%d", lastSeen)
	if err := local.getJSON(path, &stmt); err != nil {
		fatal(fmt.Errorf("build statement: %w", err))
	}

	switch {
	case lastSeen == 0:
		fmt.Printf("witness %s has never seen this node — presenting %d entries as a baseline\n",
			remote, stmt.Seq)
	case lastSeen == stmt.Seq:
		fmt.Printf("witness %s already vouched for all %d entries — re-presenting the same head\n",
			remote, stmt.Seq)
	default:
		fmt.Printf("witness %s last saw %d entries; presenting %d with a %d-hash consistency proof\n",
			remote, lastSeen, stmt.Seq, len(stmt.Consistency))
	}

	// 3. Ask it to vouch.
	cs, err := requestCountersignature(http, remote, stmt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n  REFUSED by %s\n  → %v\n\n", remote, err)
		fmt.Fprintln(os.Stderr,
			"  A refusal on the consistency check is not a network problem. It means this\n"+
				"  node's ledger no longer contains, unchanged, what that witness already\n"+
				"  signed for — which is what a rewritten history looks like from outside.")
		os.Exit(1)
	}

	// 4. Record it locally, after the node re-verifies it.
	body, _ := json.Marshal(map[string]any{
		"seq": stmt.Seq, "merkle_root": stmt.MerkleRoot,
		"witness_url": remote, "countersignature": cs,
	})
	if _, err := local.do("POST", "/v1/ledger/witness/record", body); err != nil {
		fatal(fmt.Errorf("record countersignature: %w", err))
	}

	fmt.Printf("\n  WITNESSED — %s vouched for %d entries at %s\n",
		remote, stmt.Seq, shortRoot(stmt.MerkleRoot))
	fmt.Printf("  witness key %s\n\n", ledger.Fingerprint(cs.WitnessKey))
	fmt.Println("  From here on that witness will refuse to vouch for any history of this")
	fmt.Println("  node that does not still contain these entries, unchanged and in order.")
}

// askLastSeen asks a witness how far it has already vouched for us. A witness
// that has never heard of this node answers 0, which is not an error — it is
// the first-contact case.
func askLastSeen(c *witnessClient, remote, nodeID string) (uint64, error) {
	url := fmt.Sprintf("%s/v1/ledger/witness/last-seen?node=%s", remote, nodeID)
	code, body, err := c.do("GET", url, nil)
	if err != nil {
		return 0, fmt.Errorf("reach witness %s: %w", remote, err)
	}
	if code == 401 {
		return 0, fmt.Errorf(
			"witness %s requires a bearer token — pass --token, or set "+
				"AURA_WITNESS_TOKEN (its node.token, not this node's)", remote)
	}
	if code != 200 {
		return 0, fmt.Errorf("witness %s answered %d: %s", remote, code, strings.TrimSpace(string(body)))
	}
	var out struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("witness %s returned an unreadable answer: %w", remote, err)
	}
	return out.Seq, nil
}

func requestCountersignature(c *witnessClient, remote string, stmt ledger.Statement) (ledger.Countersignature, error) {
	body, err := json.Marshal(stmt)
	if err != nil {
		return ledger.Countersignature{}, err
	}
	code, raw, err := c.do("POST", remote+"/v1/ledger/witness", body)
	if err != nil {
		return ledger.Countersignature{}, fmt.Errorf("reach witness %s: %w", remote, err)
	}
	if code != 200 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return ledger.Countersignature{}, fmt.Errorf("%s", e.Error)
		}
		return ledger.Countersignature{}, fmt.Errorf("witness answered %d: %s",
			code, strings.TrimSpace(string(raw)))
	}
	var cs ledger.Countersignature
	if err := json.Unmarshal(raw, &cs); err != nil {
		return ledger.Countersignature{}, fmt.Errorf("unreadable countersignature: %w", err)
	}
	return cs, nil
}

// A note on who may witness.
//
// `/v1/ledger/witness` sits behind the node's bearer token, like every other
// `/v1` route — only `/healthz`, `/hooks/*` and the UI's static assets are
// exempt, and each for a stated reason. That makes witnessing work today
// between nodes whose operators can exchange a token: two nodes of one org,
// two orgs that have agreed to anchor each other.
//
// It also means there is no *anonymous public* witness, which is where
// Certificate Transparency draws most of its strength: the more independent
// the witness, the more a countersignature is worth. Opening the endpoint
// would be one line, and it is deliberately not that line, because an
// unauthenticated write surface needs answers this release does not have —
// what bounds `witnessed_heads` when anyone can mint a keypair and present a
// fresh node id, and what stops that table becoming a free database. Doing it
// properly means rate limits and a retention policy, and shipping it without
// them would be trading a real guarantee for a denial-of-service.
//
// Tracked as future work rather than presented as done. See
// [Security model](../../../GUIDE.md#external-anchoring).

func shortRoot(root string) string {
	if len(root) > 22 {
		return root[:22] + "…"
	}
	return root
}
