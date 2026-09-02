package main

// Building a node, in one place.
//
// `aura up` used to be the only thing that could construct a kernel, with the
// two hundred lines that do it inlined into its flag parsing. That was fine
// while it was the only caller and stopped being fine the moment a second one
// existed: `aura guard` needs a real node — store, ledger, policy, executor,
// gateway — and copying the construction would mean two nodes that drift, where
// one of them quietly forgets to open the ledger or to refuse a policy that
// requires signed approval with an empty roster.
//
// So the construction lives here and both callers use it. What stays in `up` is
// what only `up` has: flags, a banner, a public listener, WebTransport, pprof.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"aura/kernel/internal/approvals"
	"aura/kernel/internal/approver"
	"aura/kernel/internal/broker"
	"aura/kernel/internal/executor"
	"aura/kernel/internal/gateway"
	"aura/kernel/internal/grammar"
	"aura/kernel/internal/identity"
	"aura/kernel/internal/lease"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/mcpsrv"
	"aura/kernel/internal/projection"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/store"
	"aura/kernel/internal/wasmrt"
)

// defaultMaxSessions matches the `aura up` flag default, so an embedded node
// admits exactly what a started one would. A second default here is a second
// thing to keep in step.
const defaultMaxSessions = 1000

// nodeOptions is what a caller decides about a node it is building. The zero
// value is a loopback, authenticated, default-policy node — the same posture
// `aura up` has with no flags, because a second caller getting a *laxer* default
// than the first is exactly the drift this file exists to prevent.
type nodeOptions struct {
	DataDir string
	Port    int
	Mode    string
	// PolicyPath is empty for the built-in default policy.
	PolicyPath string
	// NoAuth disables the bearer token. Off by default, which is the point:
	// a caller that wants an unauthenticated node has to say so.
	NoAuth           bool
	OpenWitness      bool
	TLS              bool
	AllowedOrigins   []string
	TrustedProxy     bool
	MemoryBudget     int64
	EventLogMaxBytes int64
	MaxSessions      int
	// LeaseTTL is how long this node's claim on the data directory stays valid
	// without a renewal. Zero takes lease.DefaultTTL.
	//
	// Measured, not guessed: after a SIGKILL, recovery is ~100% this value.
	// The node itself rebuilds in single-digit milliseconds even over a
	// 4,000-entry ledger; the rest of the wall clock is a replacement waiting
	// out the dead holder's claim. So this is the recovery-time knob, and
	// lowering it trades against false takeovers when a live node stalls.
	LeaseTTL time.Duration
	// LeaseWait bounds how long to wait for a dead predecessor's claim to
	// expire. Zero derives it from LeaseTTL. A supervisor restarting a crashed
	// node needs this to exceed the TTL, or the replacement refuses.
	LeaseWait   time.Duration
	SkillConfig map[string]map[string]any
	Log         *slog.Logger
}

// Startup is how long each phase of buildNode took.
//
// Recorded rather than guessed because it is the number that decides whether
// this runtime needs failover at all. Recovery from a crash is process start
// plus these phases plus the client's reconnect, and until somebody measured
// it the roadmap was arguing about failover without knowing whether the answer
// was two seconds or two minutes.
//
// The two that grow with the data are worth watching separately: Store covers
// the causal event log's recovery scan, and Ledger covers rebuilding the RFC
// 6962 tree over every sealed entry.
type Startup struct {
	Total     time.Duration
	LeaseWait time.Duration
	Store     time.Duration
	Ledger    time.Duration
}

// node is a constructed kernel, with everything a caller might need to serve it
// or to shut it down.
type node struct {
	Gateway  *gateway.Gateway
	Identity *identity.Node
	Store    *store.Store
	Ledger   *ledger.Ledger
	Policy   *executor.Policy
	Registry *registry.Registry
	Manager  *executor.Manager

	// WriteLease is this process's claim on the data directory. Its Lost()
	// channel is a shutdown signal, not advice: another process taking over
	// means this one must stop writing at once.
	WriteLease *lease.Holder

	// Startup is what building this node cost, phase by phase.
	Startup Startup

	// Token is the operator's bearer token, empty when NoAuth. Created reports
	// whether this run is the one that minted it.
	Token   string
	Created bool

	scheme, wsScheme string
	port             int
}

// Close releases the write lease before closing the store, so a replacement
// node can start immediately instead of waiting out the TTL. Order matters:
// the lease row lives in the database the store is about to close.
func (n *node) Close() error {
	if n.WriteLease != nil {
		if err := n.WriteLease.Release(); err != nil {
			return errors.Join(err, n.Store.Close())
		}
	}
	return n.Store.Close()
}

// BaseURL is where this node answers HTTP.
func (n *node) BaseURL() string { return fmt.Sprintf("%s://localhost:%d", n.scheme, n.Port()) }

// Port is the port the node was built for.
func (n *node) Port() int { return n.port }

// buildNode constructs a kernel. It opens files and starts no listener: the
// caller decides how the result is served, which is the difference between
// `aura up` and an embedded node inside another command.
func buildNode(opt nodeOptions) (*node, error) {
	log := opt.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	mode := opt.Mode
	if mode == "" {
		mode = "local"
	}

	policy, err := executor.LoadPolicy(opt.PolicyPath)
	if err != nil {
		return nil, fmt.Errorf("--policy: %w", err)
	}

	ident, err := identity.Load(opt.DataDir, identity.Mode(mode))
	if err != nil {
		return nil, err
	}
	buildStarted := time.Now()
	storeStarted := time.Now()
	st, err := store.OpenWith(opt.DataDir, store.Options{EventLogMaxBytes: opt.EventLogMaxBytes})
	if err != nil {
		return nil, err
	}
	timing := Startup{Store: time.Since(storeStarted)}

	// One writer per data directory. Two kernels appending to the same effect
	// ledger would interleave sequences and hash-chain links that each
	// believed it owned — not a crash, but a ledger that fails to verify with
	// no way to tell which process wrote what. Taken here, before anything
	// else can write.
	//
	// It *waits* rather than failing on a held lease. A node that was
	// SIGKILLed does not release its claim, so for up to a TTL afterwards the
	// row still names a process that no longer exists — and a replacement that
	// refused would be restarted into a crash loop that ends only when the
	// supervisor's backoff happens to exceed the TTL. Waiting is bounded: if
	// the holder is genuinely alive, this must not start.
	leaseTTL := opt.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = lease.DefaultTTL
	}
	leaseWait := opt.LeaseWait
	if leaseWait == 0 {
		leaseWait = leaseTTL + 5*time.Second
	}
	writeLease, waited, err := st.AcquireWriteLeaseWaiting(leaseTTL, leaseWait)
	timing.LeaseWait = waited
	if err != nil {
		st.Close()
		if errors.Is(err, lease.ErrHeld) {
			return nil, fmt.Errorf("%w\n\n"+
				"  Stop that node first, or point --data at a different directory.\n"+
				"  If it crashed, its lease expires within %s and this starts on its own.",
				err, leaseTTL)
		}
		return nil, err
	}

	// Anything that fails past this point has to release the lease and close
	// the store, or a failed build leaves the directory claimed and the next
	// attempt reports something unrelated — for up to a full TTL, which is a
	// deeply confusing way to learn that a policy file had a typo.
	fail := func(err error) (*node, error) {
		_ = writeLease.Release()
		st.Close()
		return nil, err
	}

	// The ledger is not optional. An attestation issued by a node that failed
	// to open its own ledger would be worse than no ledger — it would be
	// silently absent while everything else ran normally.
	ledgerStarted := time.Now()
	ldg, err := ledger.Open(st, ident.ID, ident.Keys)
	if err != nil {
		return fail(fmt.Errorf("open effect ledger: %w", err))
	}
	timing.Ledger = time.Since(ledgerStarted)

	// The wasm sandbox. Unlike the ledger this is allowed to fail soft: a node
	// that cannot link WASI preview1 still runs every other primitive fine.
	wasmRT, err := wasmrt.New(context.Background())
	if err != nil {
		log.Warn("wasm runtime unavailable — format:wasm skills cannot be hosted", "err", err)
	}

	// The token is minted before anything is served, so there is no window in
	// which the node is reachable without one.
	var auth *gateway.Auth
	var token string
	var created bool
	if !opt.NoAuth {
		token, created, err = gateway.LoadOrCreateToken(opt.DataDir)
		if err != nil {
			return fail(err)
		}
		auth = &gateway.Auth{
			Token:          token,
			Tokens:         tokenResolver{st},
			AllowedOrigins: opt.AllowedOrigins,
			TrustedProxy:   opt.TrustedProxy,
			OpenWitness:    opt.OpenWitness,
		}
	}

	scheme, wsScheme := "http", "ws"
	if opt.TLS {
		scheme, wsScheme = "https", "wss"
	}

	roster, err := approver.Load(st)
	if err != nil {
		return fail(fmt.Errorf("load operator roster: %w", err))
	}
	brk, err := broker.Open(st, ident.Keys)
	if err != nil {
		return fail(fmt.Errorf("open credential broker: %w", err))
	}

	// Refused at build rather than at the first gated effect. A node that
	// requires signed approval with nobody enrolled can answer no gate at all,
	// so it would come up healthy and then deny its first write minutes later.
	if policy.SignedApprovalRequired() && roster.Empty() {
		asked := "require_signed_approval"
		if len(policy.ApprovalContextRequired()) > 0 {
			// Naming the setting the operator actually wrote. require_approval_context
			// implies the other one, so reporting the implied name would send them
			// looking for a line their file does not contain.
			asked = "require_approval_context"
		}
		return fail(fmt.Errorf("policy %s sets %s but no operator is enrolled — "+
			"nobody could answer a gate, so every gated effect would be denied; "+
			"enrol someone with `aura operator enroll <id>`", policy.Source(), asked))
	}

	reg := registry.New()
	mgr := executor.NewManager(reg, st, mode, policy, ldg, log)
	mgr.SetMaxSessions(opt.MaxSessions)
	mgr.SetApprovers(roster)

	proj := projection.NewManager(
		fmt.Sprintf("%s://localhost:%d/ws/skill", wsScheme, opt.Port), log)
	proj.Token = token
	proj.Secrets = brk

	// One approval queue, shared: the MCP server parks gates in it and the
	// /v1/approvals routes answer them. Without a shared instance a gate raised
	// over MCP would be invisible to the surface meant to resolve it.
	appr := approvals.New()
	appr.Require(policy.ApprovalContextRequired())
	mcp := &mcpsrv.Server{
		BaseURL: fmt.Sprintf("%s://localhost:%d", scheme, opt.Port),
		Version: version, Token: token, Log: log, Approvals: appr,
		NodeID: ident.ID,
	}

	gw := &gateway.Gateway{
		Node: ident, Reg: reg, St: st, Mgr: mgr, Proj: proj,
		Adm: gateway.NewAdmission(opt.MemoryBudget), Ldg: ldg, Wasm: wasmRT,
		MCP: mcp.Handler(), Log: log, Auth: auth,
		Approvals: appr, Broker: brk,
		// Every node with an identity can witness for others. It costs nothing
		// when unused and means a two-node deployment already has somewhere to
		// anchor, rather than needing a service nobody has stood up yet.
		Wit: witnessService(st, ident, opt.OpenWitness, log),
		// Typed ports made enforceable: a grammar per port schema, so a skill
		// that generates cannot emit a shape the port would reject.
		Grammars:   grammar.NewRegistry(),
		ConfigFile: opt.SkillConfig,
	}

	timing.Total = time.Since(buildStarted)
	return &node{
		WriteLease: writeLease,
		Startup:    timing,
		Gateway:    gw, Identity: ident, Store: st, Ledger: ldg, Policy: policy,
		Registry: reg, Manager: mgr,
		Token: token, Created: created,
		scheme: scheme, wsScheme: wsScheme, port: opt.Port,
	}, nil
}

// nopWriter swallows log output for a caller that did not ask for any. An
// embedded node inside another command should not interleave its slog lines
// with that command's own output unless the operator asked for them.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// startEmbeddedNode builds a kernel and serves it on loopback, for a command
// that needs a node rather than a long-running one an operator started.
//
// Loopback, always. An embedded node exists for the duration of one command and
// nobody outside this machine has a reason to reach it; binding wider would be a
// surprise an operator never asked for. Everything else — the policy, the
// ledger, the token — is what `aura up` would have produced from the same data
// directory, because it is the same construction.
func startEmbeddedNode(dataDir string, port int, policyPath string) (*node, *http.Server, error) {
	n, err := buildNode(nodeOptions{
		DataDir: dataDir, Port: port, PolicyPath: policyPath,
		MaxSessions: defaultMaxSessions,
	})
	if err != nil {
		return nil, nil, err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		n.Close()
		return nil, nil, fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
	}
	srv := &http.Server{
		Handler:           n.Gateway.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			n.Gateway.Log.Error("embedded node stopped serving", "err", err)
		}
	}()
	// The guard dials straight back in, so the listener has to be accepting
	// before this returns rather than "shortly after".
	if err := waitForHealth(n.BaseURL(), 10*time.Second); err != nil {
		srv.Close()
		n.Close()
		return nil, nil, err
	}
	return n, srv, nil
}

// waitForHealth blocks until the node answers, or gives up.
func waitForHealth(base string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("the embedded node did not become healthy within %s", limit)
}
