package main

// `aura guard` — put the MCP servers an agent already uses behind this node.
//
// The command is deliberately thin. It reads the `mcpServers` document the
// operator already has, hands it to internal/guard, and prints what got
// registered. Every decision that matters afterwards — may this call happen, does
// it need a human, what gets sealed — belongs to the node policy and the
// executor, which is the point of routing through them rather than around.
//
// The printed table is the deliverable as much as the process is: an operator
// pointing an agent at a guarded node should be able to see, before anything
// runs, exactly which tools became gated and which did not.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"aura/kernel/internal/approvals"
	"aura/kernel/internal/guard"
)

func cmdGuard(args []string) {
	fs := flag.NewFlagSet("guard", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to an mcpServers JSON document (required)")
	port := fs.Int("port", 9080, "kernel port")
	dryRun := fs.Bool("dry-run", false, "connect and report what would be registered, then exit")
	trust := fs.Bool("trust-annotations", false,
		"believe a server's readOnlyHint and leave those tools ungated")
	standalone := fs.Bool("standalone", false,
		"run an embedded kernel instead of connecting to one (default when no node answers)")
	data := fs.String("data", defaultDataDir(), "data directory for the embedded kernel")
	policyPath := fs.String("policy", "", "authorization policy for the embedded kernel")
	_ = parseWithOperands(fs, args, 1)

	path := *cfgPath
	if path == "" && len(fs.Args()) > 0 {
		path = fs.Args()[0]
	}
	if path == "" {
		fatal(fmt.Errorf("usage: aura guard --config <mcp-servers.json> [--port 9080] [--dry-run]\n\n" +
			"The config is the same \"mcpServers\" document Claude Desktop, Cursor and\n" +
			"the rest already write — point this at the file you have."))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal(fmt.Errorf("read %s: %w", path, err))
	}
	cfg, err := guard.ParseConfig(raw)
	if err != nil {
		fatal(err)
	}

	// Reaching a node, or becoming one.
	//
	// `aura guard` used to require `aura up` first, which made the shortest path
	// to a guarded agent two processes and a port. That is the wrong shape for
	// what this command is *for*: it exists so that someone who has not adopted
	// this runtime can put their agent's existing tools behind a policy, a gate
	// and a ledger — and asking them to stand up a runtime first is asking them
	// to adopt it.
	//
	// So an unreachable node is not an error unless the operator pinned one.
	// The embedded kernel is the same kernel: same construction (see node.go),
	// same policy, same ledger in the same data directory, so `aura verify`
	// afterwards reads exactly what a long-running node would have written.
	var (
		baseURL, token, nodeID string
		embedded               *node
	)
	c := newNodeClient(*port)
	var health struct {
		Node string `json:"node"`
		Mode string `json:"mode"`
	}
	reachErr := c.getJSON("/healthz", &health)

	switch {
	case reachErr == nil && *standalone:
		fatal(fmt.Errorf("a node is already answering on port %d — "+
			"drop --standalone to guard through it, or pick a free port", *port))
	case reachErr == nil:
		baseURL, token, nodeID = c.base, c.token, health.Node
	default:
		n, srv, err := startEmbeddedNode(*data, *port, *policyPath)
		if err != nil {
			fatal(fmt.Errorf("no node answered on port %d and an embedded one could not start: %w",
				*port, err))
		}
		defer n.Close()
		defer srv.Close()
		embedded, baseURL, token, nodeID = n, n.BaseURL(), n.Token, n.Identity.ID
		fmt.Printf("\n  no node on port %d — running an embedded kernel\n", *port)
		fmt.Printf("  node      %s\n  data      %s\n", n.Identity.ID, *data)
		fmt.Printf("  policy    %s\n", n.Policy.Source())
		fmt.Printf("  ledger    every guarded call is sealed here — "+
			"`aura verify --data %s` reads it with no node running\n\n", *data)
	}
	// Held only so the deferred Close above is not the sole reference; the
	// guard talks to the node over HTTP like any other client, embedded or not.
	_ = embedded

	g := guard.New(cfg, guard.Options{
		BaseURL: baseURL, Token: token,
		Log:              slog.New(slog.NewTextHandler(os.Stdout, nil)),
		TrustAnnotations: *trust, DryRun: *dryRun,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *dryRun {
		if err := g.Run(ctx); err != nil {
			fatal(err)
		}
		printBindings(g.Bindings(), nodeID, true)
		return
	}

	// Run blocks until the context ends; the summary has to be printed from
	// alongside it, once registration has had a moment to land.
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			fatal(err)
		}
		return
	case <-waitForBindings(g):
	}
	printBindings(g.Bindings(), nodeID, false)

	select {
	case err := <-done:
		if err != nil {
			fatal(err)
		}
	case <-ctx.Done():
		<-done
	}
	fmt.Println("\n  guard stopped — the agent's tools are unguarded again.")
}

// waitForBindings closes once the guard has finished its first pass, so the
// summary describes a real registration rather than an empty slice.
func waitForBindings(g *guard.Guard) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		for i := 0; i < 600; i++ {
			if len(g.Bindings()) > 0 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return ch
}

func printBindings(bs []guard.Binding, node string, dry bool) {
	if len(bs) == 0 {
		fmt.Println("  no tools were registered — every upstream server was unreachable or empty.")
		return
	}
	sort.Slice(bs, func(i, j int) bool {
		if bs[i].Server != bs[j].Server {
			return bs[i].Server < bs[j].Server
		}
		return bs[i].Tool < bs[j].Tool
	})

	gated := 0
	for _, b := range bs {
		if b.Gated() {
			gated++
		}
	}
	verb := "guarding"
	if dry {
		verb = "would guard"
	}
	fmt.Printf("\n  aura guard — %s %d tool(s) on node %s\n\n", verb, len(bs), node)

	wTool, wCap := len("TOOL"), len("CAPABILITY")
	for _, b := range bs {
		if n := len(b.Server + "/" + b.Tool); n > wTool {
			wTool = n
		}
		if n := len(b.Capability); n > wCap {
			wCap = n
		}
	}
	fmt.Printf("  %-*s  %-*s  %s\n", wTool, "TOOL", wCap, "CAPABILITY", "ON CALL")
	fmt.Printf("  %s  %s  %s\n", strings.Repeat("-", wTool), strings.Repeat("-", wCap), strings.Repeat("-", 24))
	for _, b := range bs {
		posture := "sealed only"
		if b.Gated() {
			posture = "human approval + sealed"
		}
		fmt.Printf("  %-*s  %-*s  %s\n", wTool, b.Server+"/"+b.Tool, wCap, b.Capability, posture)
	}

	fmt.Printf("\n  %d of %d act on the world and are gated; the rest are read-only.\n", gated, len(bs))
	if dry {
		fmt.Println("  --dry-run: nothing was registered.")
		return
	}
	fmt.Println("\n  Point the agent at this node instead of the servers directly:")
	fmt.Println("    claude mcp add --transport http aura http://localhost:9080/mcp")
	fmt.Println("\n  Approve gated calls with `aura approve`, the control-plane UI, or")
	fmt.Println("  POST /v1/approvals/{id}. Ctrl-C stops guarding.")
}

// ── approvals ───────────────────────────────────────────────────────

func cmdApprovals(args []string) {
	fs := flag.NewFlagSet("approvals", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	asJSON := fs.Bool("json", false, "emit the raw list")
	_ = fs.Parse(args)

	c := newNodeClient(*port)
	body, err := c.do("GET", "/v1/approvals", nil)
	if err != nil {
		fatal(err)
	}
	if *asJSON {
		fmt.Println(strings.TrimSpace(string(body)))
		return
	}
	var out struct {
		Approvals []approvals.Pending `json:"approvals"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		fatal(err)
	}
	if len(out.Approvals) == 0 {
		fmt.Println("  nothing is waiting for approval.")
		return
	}
	fmt.Printf("\n  %d call(s) waiting:\n\n", len(out.Approvals))
	for _, p := range out.Approvals {
		fmt.Printf("  %s\n", p.ID)
		fmt.Printf("      %s\n", p.Question)
		if p.Tool != "" {
			fmt.Printf("      tool     %s (via %s)\n", p.Tool, p.Origin)
		}
		if len(p.Arguments) > 0 && string(p.Arguments) != "{}" && string(p.Arguments) != "null" {
			fmt.Printf("      args     %s\n", truncate(string(p.Arguments), 160))
		}
		if len(p.ContextRequired) > 0 {
			fmt.Printf("      binds    %s (--shown <label>=<file>)\n", strings.Join(p.ContextRequired, ", "))
		}
		fmt.Printf("      expires  %s\n\n", p.Expires.Format("15:04:05"))
	}
	fmt.Println("  aura approve <id>            allow it")
	fmt.Println("  aura approve <id> --deny     refuse it")
}

func cmdApprove(args []string) {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	deny := fs.Bool("deny", false, "refuse the call instead of allowing it")
	as := fs.String("as", "", "sign the answer as this enrolled operator (C4 v1.3)")
	var shownFiles, shownDigests stringList
	fs.Var(&shownFiles, "shown", "<label>=<path>: hash this file and bind it into the signature (C4 v1.7); repeatable")
	fs.Var(&shownDigests, "shown-digest", "<label>=<alg>:<hex>: bind a digest computed elsewhere; repeatable")
	ops := parseWithOperands(fs, args, 1)
	if len(ops) == 0 {
		fatal(fmt.Errorf("usage: aura approve <approval-id> [--as <operator>] [--deny]\n" +
			"                    [--shown <label>=<path>] [--shown-digest <label>=<alg>:<hex>] [--port 9080]\n\n" +
			"List what is waiting with `aura approvals`.\n" +
			"With --as, the answer is signed with that operator's key and sealed into\n" +
			"the ledger entry, so the record says who allowed it and not merely that\n" +
			"somebody did.\n\n" +
			"With --shown, the signature also covers what you were looking at:\n\n" +
			"  aura approve 01J9… --as grace --shown screen=./note.html\n\n" +
			"The file is hashed here, on your side, and only the hash is sent. A node\n" +
			"whose policy lists require_approval_context refuses an answer that binds\n" +
			"nothing; `aura approvals` prints which labels it wants."))
	}

	answer := map[string]any{"approve": !*deny}
	shown, err := gatherShown(shownFiles, shownDigests)
	if err != nil {
		fatal(err)
	}
	if *as == "" && len(shown) > 0 {
		// An unsigned answer has nowhere to put a context: it is carried inside
		// the operator's signature. Accepting the flag and dropping it would
		// leave someone believing they had bound what they read.
		fatal(fmt.Errorf("--shown needs --as: what you were shown is carried inside your signature, " +
			"and an unsigned answer has no signature to carry it"))
	}
	if *as != "" {
		signed, err := signApprovalFor(*port, ops[0], *as, !*deny, shown)
		if err != nil {
			fatal(err)
		}
		answer["approval"] = signed
	}

	c := newNodeClient(*port)
	body, _ := json.Marshal(answer)
	if _, err := c.do("POST", "/v1/approvals/"+ops[0], body); err != nil {
		fatal(err)
	}
	verb := "approved"
	if *deny {
		verb = "denied"
	}
	if *as != "" {
		line := fmt.Sprintf("  %s %s — signed as %s", verb, ops[0], *as)
		if len(shown) > 0 {
			labels := make([]string, 0, len(shown))
			for _, e := range shown {
				labels = append(labels, e.Label)
			}
			line += ", binding " + strings.Join(labels, ", ")
		}
		fmt.Println(line)
		return
	}
	fmt.Printf("  %s %s\n", verb, ops[0])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
