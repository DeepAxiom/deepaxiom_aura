// aura — single-binary kernel implementing the AURA architecture.
//
//	aura up        start the kernel on this machine
//	aura status    query a running kernel
//	aura version   print version and protocol majors
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aura/kernel/internal/approvals"
	"aura/kernel/internal/channel"
	"aura/kernel/internal/config"
	"aura/kernel/internal/executor"
	"aura/kernel/internal/gateway"
	"aura/kernel/internal/grammar"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/mcpsrv"
	"aura/kernel/internal/projection"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
	"aura/kernel/internal/store"
	"aura/kernel/internal/wasmrt"
	"aura/kernel/internal/wtsrv"
)

// version comes from spec/VERSION via the generated spec package. It used to
// be a literal here and a different literal in the agent card.
const version = spec.Version

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "up":
		cmdUp(os.Args[2:])
	case "chat":
		cmdChat(os.Args[2:])
	case "do":
		cmdDo(os.Args[2:])
	case "connect":
		cmdConnect(os.Args[2:])
	case "observe":
		cmdObserve(os.Args[2:])
	case "generate":
		cmdGenerate(os.Args[2:])
	case "projections":
		cmdProjections(os.Args[2:])
	case "promote":
		cmdPromote(os.Args[2:])
	case "why":
		cmdWhy(os.Args[2:])
	case "replay":
		cmdReplay(os.Args[2:])
	case "trace":
		cmdTrace(os.Args[2:])
	case "federate":
		cmdFederate(os.Args[2:])
	case "registry":
		cmdRegistry(os.Args[2:])
	case "publish":
		cmdPublish(os.Args[2:])
	case "add":
		cmdAdd(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "witness":
		cmdWitness(os.Args[2:])
	case "receipt":
		cmdReceipt(os.Args[2:])
	case "bom":
		cmdBOM(os.Args[2:])
	case "bundle":
		cmdBundle(os.Args[2:])
	case "undo":
		cmdUndo(os.Args[2:])
	case "guard":
		cmdGuard(os.Args[2:])
	case "approve":
		cmdApprove(os.Args[2:])
	case "approvals":
		cmdApprovals(os.Args[2:])
	case "version":
		fmt.Printf("aura %s (channel protocol %s, graph ir %s)\n",
			version, channel.ProtocolMajor, executor.IRMajor)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `aura — cognitive runtime kernel

Usage:
  aura up [--port 9080] [--data <dir>] [--mode local|site|published]
          [--memory-budget 8Gi] [--config <file>] [--open-witness]
  aura chat ["message"] [--graph chat] [--port 9080]
  aura do "natural-language goal" [--yes] [--port 9080]
  aura connect --openapi <url|file> [--name x] [--base-url y] [--header "K: V"]
  aura observe --target <url> [--port 8080] [--out observed.jsonl]
  aura generate connector --from <recording> --name <name> [--base-url y] [--out f]
  aura projections [--port 9080]
  aura promote <projection> <op> --mode dry-run|live|disabled
  aura why [session] [--no-explain] [--port 9080]
  aura replay <session> [--graph <id>] [--deny-gates] [--port 9080]
  aura trace <session> [--otlp <url>] [--out <file>] [--port 9080]
  aura federate <remote-url> [--capability <cap>] [--port 9080]
  aura registry serve [--port 9091] [--data <dir>]
  aura publish <skill-dir> [--registry <url>]
  aura add <org/cat/name>[@version] | --capability <cap> [--registry <url>] [--yes]
  aura run <org/cat/name> [--port 9080]
  aura status [--port 9080]
  aura verify [--data <dir>]
  aura witness <witness-url> [--token <t>] [--port 9080]
  aura receipt <effect-hash> [--out <f>] | --verify <f>
  aura bundle <session> [--out <f>] | --verify <f>
  aura bom [session] [--out <f>]
  aura undo <session|receipt> [--yes] [--port 9080]
  aura guard --config <mcp-servers.json> [--dry-run] [--trust-annotations]
  aura approvals [--json] [--port 9080]
  aura approve <id> [--deny] [--port 9080]
  aura version`)
}

// witnessService builds this node's witnessing role. An open witness gets the
// default bounds and a background pruner; a closed one needs neither, since
// every caller already holds a credential.
func witnessService(st *store.Store, node *identity.Node, open bool, log *slog.Logger) *ledger.Witness {
	w := ledger.NewWitness(st, node.Keys)
	if !open {
		return w
	}
	limits := ledger.DefaultWitnessLimits()
	w.Open(limits)
	log.Warn("open witness enabled — any node may anchor its ledger here without a token",
		"max_nodes", limits.MaxNodes, "per_node_per_hour", limits.PerNodePerHour,
		"retention_days", limits.RetentionDays)
	go func() {
		// Retention is enforced on a slow timer rather than per request: a
		// caller should never pay for someone else's housekeeping.
		for range time.Tick(6 * time.Hour) {
			if dropped, err := w.Prune(); err != nil {
				log.Error("witness prune failed", "err", err)
			} else if dropped > 0 {
				log.Info("witness forgot nodes past retention", "dropped", dropped)
			}
		}
	}()
	return w
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aura"
	}
	return filepath.Join(home, ".aura")
}

// stringList collects a repeatable flag (--allow-origin a --allow-origin b).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	port := fs.Int("port", 9080, "HTTP+WS port")
	listen := fs.String("listen", "", "address to bind (default loopback; \"0.0.0.0\" exposes the node)")
	data := fs.String("data", defaultDataDir(), "data directory")
	mode := fs.String("mode", "local", "security mode: local|site|published")
	memBudget := fs.String("memory-budget", "", "R15 admission budget, e.g. 8Gi (empty = unlimited)")
	configPath := fs.String("config", "", "optional skill-config file (skill defaults, git-friendly) — see README.md")
	policyPath := fs.String("policy", "", "authorization policy file (default: built-in permissive policy)")
	maxSessions := fs.Int("max-sessions", 1000, "cap on concurrent live sessions (0 = unlimited)")
	noAuth := fs.Bool("no-auth", false, "disable the bearer token (single-user loopback nodes only)")
	pprofAddr := fs.String("pprof", "", "expose Go profiling on this address (e.g. 127.0.0.1:6060); off by default")
	openWitness := fs.Bool("open-witness", false,
		"let any node anchor its ledger here without a token (rate- and capacity-bounded)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (enables HTTPS/WSS)")
	tlsKey := fs.String("tls-key", "", "TLS private key file")
	var allowOrigins stringList
	fs.Var(&allowOrigins, "allow-origin", "extra browser Origin allowed on the WebSocket upgrade (repeatable)")
	_ = fs.Parse(args)

	budgetBytes, err := gateway.ParseMemory(*memBudget)
	if err != nil {
		fatal(fmt.Errorf("--memory-budget: %w", err))
	}

	configFile, err := config.Load(*configPath)
	if err != nil {
		fatal(fmt.Errorf("--config: %w", err))
	}

	policy, err := executor.LoadPolicy(*policyPath)
	if err != nil {
		fatal(fmt.Errorf("--policy: %w", err))
	}

	addr, public, err := gateway.ResolveListen(*listen, *port)
	if err != nil {
		fatal(err)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		fatal(fmt.Errorf("--tls-cert and --tls-key must be given together"))
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	node, err := identity.Load(*data, identity.Mode(*mode))
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(*data)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	// The ledger is not optional. An attestation issued by a node that failed
	// to open its own ledger would be worse than no ledger — it would be
	// silently absent while everything else ran normally — so a failure here
	// stops the node the same way a failed store.Open does.
	ldg, err := ledger.Open(st, node.ID, node.Keys)
	if err != nil {
		fatal(fmt.Errorf("open effect ledger: %w", err))
	}

	// The wasm sandbox (Phase 3). Unlike the ledger this is allowed to fail
	// soft: a node that cannot link WASI preview1 (should never happen on a
	// supported platform, but the failure mode matters) still runs every
	// other primitive fine — format:wasm skills simply cannot register,
	// exactly like a node with --no-auth still runs without a token.
	wasmRT, err := wasmrt.New(context.Background())
	if err != nil {
		log.Warn("wasm runtime unavailable — format:wasm skills cannot be hosted", "err", err)
	}

	// The token is minted before anything is served, so there is no window in
	// which the node is reachable without one.
	var auth *gateway.Auth
	var token string
	var tokenCreated bool
	if !*noAuth {
		token, tokenCreated, err = gateway.LoadOrCreateToken(*data)
		if err != nil {
			fatal(err)
		}
		auth = &gateway.Auth{
			Token:          token,
			AllowedOrigins: allowOrigins,
			TrustedProxy:   *tlsCert == "" && public,
			OpenWitness:    *openWitness,
		}
	}

	scheme := "http"
	wsScheme := "ws"
	if *tlsCert != "" {
		scheme, wsScheme = "https", "wss"
	}

	reg := registry.New()
	mgr := executor.NewManager(reg, st, string(node.Mode), policy, ldg, log)
	mgr.SetMaxSessions(*maxSessions)
	proj := projection.NewManager(
		fmt.Sprintf("%s://localhost:%d/ws/skill", wsScheme, *port), log)
	proj.Token = token
	adm := gateway.NewAdmission(budgetBytes)
	// One approval queue, shared: the MCP server parks gates in it and the
	// /v1/approvals routes answer them. Without a shared instance a gate raised
	// over MCP would be invisible to the surface meant to resolve it.
	appr := approvals.New()
	mcp := &mcpsrv.Server{
		BaseURL: fmt.Sprintf("%s://localhost:%d", scheme, *port),
		Version: version, Token: token, Log: log, Approvals: appr,
	}
	gw := &gateway.Gateway{Node: node, Reg: reg, St: st, Mgr: mgr, Proj: proj,
		Adm: adm, Ldg: ldg, Wasm: wasmRT, MCP: mcp.Handler(), Log: log, Auth: auth,
		Approvals: appr,
		// Every node with an identity can witness for others (C4 v1.2). It
		// costs nothing when unused and means a two-node deployment already
		// has somewhere to anchor, rather than needing a service nobody has
		// stood up yet — which is how external anchoring usually dies.
		Wit: witnessService(st, node, *openWitness, log),
		// Typed ports made enforceable: a grammar per port schema, so a skill
		// that generates cannot emit a shape the port would reject.
		Grammars:   grammar.NewRegistry(),
		ConfigFile: configFile.Skills}

	// WebTransport, beside the TCP listener rather than instead of it.
	//
	// Same port number, different protocol: QUIC is UDP, so the two do not
	// collide. A peer that can reach UDP gets the three QoS classes as three
	// real transport primitives — realtime as datagrams that cannot be stalled
	// by a lost packet, bulk on a stream nobody is waiting on. A peer that
	// cannot (corporate networks block UDP often enough that this has to be
	// assumed) keeps the WebSocket path, unchanged.
	//
	// A failure here is logged and survived: losing the faster transport is a
	// degradation, and taking the node down over it would turn a degradation
	// into an outage.
	wt := &wtsrv.Server{
		Addr:    addr,
		DataDir: *data,
		Log:     log,
		Routes: map[string]wtsrv.Handler{
			"/ws/skill": func(_ context.Context, s *wtsrv.Session, _ *http.Request) {
				gw.ServeSkill(s)
			},
		},
	}
	if err := wt.Start(); err != nil {
		log.Warn("webtransport unavailable; the WebSocket path is unaffected", "err", err)
		wt = nil
	} else {
		defer wt.Close()
	}

	// Profiling, opt-in and on its own listener.
	//
	// Separate from the node port on purpose: net/http/pprof registers on
	// DefaultServeMux and exposes heap, goroutine and CPU profiles with no
	// authentication. Hanging that off the kernel's own mux would put an
	// unauthenticated memory dump on the same port as the control plane.
	if *pprofAddr != "" {
		go func() {
			log.Warn("pprof listener enabled — unauthenticated; bind it to loopback only",
				"addr", *pprofAddr)
			mux := http.NewServeMux()
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			srv := &http.Server{Addr: *pprofAddr, Handler: mux,
				ReadHeaderTimeout: 15 * time.Second}
			if err := srv.ListenAndServe(); err != nil {
				log.Error("pprof listener stopped", "err", err)
			}
		}()
	}

	seedDefaultGraphs(st, log)
	// Projection hosts dial back into this same server; their retry loop
	// tolerates the listener not being up yet.
	go gw.StartSavedProjections()

	printBanner(bannerInfo{
		version: version, node: node, addr: addr, port: *port,
		scheme: scheme, wsScheme: wsScheme, data: *data,
		budget: budgetBytes, policy: policy, ldg: ldg, token: token,
		tokenCreated: tokenCreated, public: public, tls: *tlsCert != "",
		quic: quicEndpoint(wt),
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	if *tlsCert != "" {
		err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil {
		fatal(err)
	}
}

type bannerInfo struct {
	version      string
	node         *identity.Node
	addr         string
	port         int
	scheme       string
	wsScheme     string
	quic         string
	data         string
	budget       int64
	policy       *executor.Policy
	ldg          *ledger.Ledger
	token        string
	tokenCreated bool
	public       bool
	tls          bool
}

// printBanner is the one place a running node explains its own security
// posture. It exists because the old banner said nothing about it: a node with
// no authentication listening on every interface looked exactly like a locked
// down one, so nobody could tell which they had started.
func printBanner(b bannerInfo) {
	fmt.Printf(`
  aura %s — kernel up
  node      %s   (mode: %s)
  listen    %s
  ui        %s://localhost:%d
  skills    %s://localhost:%d/ws/skill
  clients   %s://localhost:%d/v1/stream?graph=<graph_id>
  mcp       %s://localhost:%d/mcp   (skills as MCP tools)
  quic      %s
  data      %s
  policy    %s
            %s
`, b.version, b.node.ID, b.node.Mode, b.addr,
		b.scheme, b.port, b.wsScheme, b.port, b.wsScheme, b.port, b.scheme, b.port,
		b.quic, b.data, b.policy.Source(), b.policy.Hash())

	if b.ldg != nil {
		if sum, err := b.ldg.Summarize(); err == nil {
			fmt.Printf("  ledger    %d effect(s) sealed · %d checkpoint(s) · key %s\n",
				sum.Entries, sum.Checkpoints, ledger.Fingerprint(sum.NodePubkey))
		}
	}

	if b.budget > 0 {
		fmt.Printf("  budget    %s (R15 admission)\n", gateway.FormatBytes(b.budget))
	}

	if b.token == "" {
		fmt.Printf(`
  !! NO AUTHENTICATION (--no-auth). Anyone who can reach %s can register
     a graph and run it. Only sane on a loopback-bound, single-user node.
`, b.addr)
	} else {
		verb := "reusing token from"
		if b.tokenCreated {
			verb = "new token written to"
		}
		fmt.Printf(`
  auth      bearer token (%s %s)
  open      %s://localhost:%d/#token=%s
`, verb, filepath.Join(b.data, "node.token"), b.scheme, b.port, b.token)
	}

	if b.public && !b.tls {
		fmt.Printf(`
  !! Bound to %s WITHOUT TLS. The token and every message cross the
     network in clear text. Use --tls-cert/--tls-key, or put a terminating
     proxy in front and bind loopback.
`, b.addr)
	}
	fmt.Println()
}

// seedDefaultGraphs ships the distro's out-of-the-box graphs (R8: it must
// do something useful the minute it starts).
func seedDefaultGraphs(st *store.Store, log *slog.Logger) {
	echo := map[string]any{
		"ir": executor.IRMajor, "graph_id": "echo",
		"origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{
			{"ref": "eco", "resolve": "logical.echo"},
		},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "eco.text_in"},
			{"from": "eco.text_out", "to": "client.text_in"},
		},
	}
	chat := map[string]any{
		"ir": executor.IRMajor, "graph_id": "chat",
		"origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{
			{"ref": "llm", "resolve": "cognitive.llm.chat"},
		},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "llm.text_in"},
			{"from": "llm.text_out", "to": "client.text_in"},
			{"from": "llm.status_out", "to": "client.text_in"},
		},
	}
	plan := map[string]any{
		"ir": executor.IRMajor, "graph_id": "plan",
		"origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{
			{"ref": "planner", "resolve": "cognitive.planner"},
		},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "planner.goal_in"},
			{"from": "planner.plan_out", "to": "client.text_in"},
			{"from": "planner.status_out", "to": "client.text_in"},
		},
	}
	// The voice loop: speech in, reasoning, speech out. Five client ports on
	// one socket at once, which is what "multi-channel" means in practice.
	voice := map[string]any{
		"ir": executor.IRMajor, "graph_id": "voice",
		"origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{
			{"ref": "ears", "resolve": "sensorial.asr.transcribe"},
			{"ref": "brain", "resolve": "cognitive.llm.chat"},
			{"ref": "clause", "resolve": "logical.text.sentence_chunk"},
			{"ref": "mouth", "resolve": "motor.tts.speak"},
		},
		"edges": []map[string]any{
			// Uplink stays reliable: it is only ~45 KB/s, and silently
			// dropping input audio degrades recognition invisibly, which is
			// far worse than making the sender wait.
			{"from": "client.audio_out", "to": "ears.audio_chunk_in"},
			// Only the settled transcript reaches the model. Partials are a
			// different schema on a different port for exactly this reason.
			{"from": "ears.text_out", "to": "brain.text_in"},
			{"from": "brain.text_out", "to": "clause.text_in"},
			// Speaking is a motor action, so the kernel would gate it. Say so
			// out loud: an assistant that asked permission before every
			// spoken clause would not be one.
			{"from": "clause.text_out", "to": "mouth.text_in", "gate": executor.GateNone},
			// Downlinks are realtime: a stale frame is worthless, and none of
			// these may block a skill's read loop.
			{"from": "mouth.audio_chunk_out", "to": "client.audio_in", "qos": channel.QoSRealtime},
			{"from": "ears.transcript_out", "to": "client.transcript_in", "qos": channel.QoSRealtime},
			{"from": "brain.text_out", "to": "client.text_in", "qos": channel.QoSRealtime},
			{"from": "ears.status_out", "to": "client.status_in"},
			{"from": "mouth.status_out", "to": "client.status_in"},
		},
	}
	for _, g := range []map[string]any{echo, chat, plan, voice} {
		raw, _ := json.Marshal(g)
		if err := st.SaveGraph(g["graph_id"].(string), raw); err != nil {
			log.Error("seed graph failed", "graph", g["graph_id"], "err", err)
		}
	}
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	_ = fs.Parse(args)

	c := newNodeClient(*port)
	health, err := c.do(http.MethodGet, "/healthz", nil)
	if err != nil {
		fatal(err)
	}
	fmt.Println(strings.TrimSpace(string(health)))

	// The catalog needs the token; /healthz does not. Reporting the failure
	// rather than swallowing it is the point — "0 skills" and "you are not
	// authenticated" are very different things to be told, and a raw
	// http.Get here used to conflate them by silently printing nothing.
	raw, err := c.do(http.MethodGet, "/v1/skills", nil)
	if err != nil {
		fmt.Println("skills: " + err.Error())
		return
	}
	var skills []registry.Manifest
	if json.Unmarshal(raw, &skills) == nil {
		fmt.Printf("skills connected: %d\n", len(skills))
		for _, s := range skills {
			fmt.Printf("  · %-40s %-10s %s\n", s.ID, s.Type, s.Capability)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// quicEndpoint describes the WebTransport listener for the startup banner, or
// says plainly that there is none. A node that silently lacks the faster
// transport is one whose operator debugs the wrong thing later.
func quicEndpoint(wt *wtsrv.Server) string {
	if wt == nil {
		return "unavailable (UDP blocked or in use) — WebSocket only"
	}
	return fmt.Sprintf("%s  (WebTransport: realtime→datagrams, bulk→own stream)", wt.LocalAddr())
}
