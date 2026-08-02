// Package guard puts an agent's MCP tool calls behind the kernel's checkpoint.
//
// The shape, because it is the whole idea:
//
//	agent ──▶ aura /mcp ──▶ executor ──▶ guard ──▶ the real MCP server
//	                          │
//	                    policy · gate · ledger
//
// An agent that talks to an MCP server directly gets whatever that server will
// do, with no record. Pointed at this node instead, the same call becomes an
// ordinary aura delivery: the node policy decides whether it may happen, an
// edge into a `motor` capability carries a human-approval gate the executor
// applies, and the effect is sealed into the hash-chained ledger with a
// portable receipt. None of that is implemented here — that is the point.
// Guard writes no policy code, no gate code and no ledger code, because those
// already exist one layer up and a second copy of them would be a second
// checkpoint.
//
// What guard does is narrower: it makes each upstream tool look like a skill.
// It connects to the node over `/ws/skill` — the same inversion-of-control
// socket any out-of-process skill uses, holding no kernel privileges — and
// registers one skill per tool. From the executor's side there is nothing
// special about them.
//
// # How a tool is typed, and why the default is strict
//
// The classification decides whether a call is gated, so it is the only
// judgement guard makes. MCP servers may annotate a tool `readOnlyHint`. A tool
// that claims to be read-only is registered `sensorial`; everything else —
// including every tool on a server that annotates nothing at all — is
// registered `motor`, which is what makes the executor demand approval before
// it runs.
//
// That default is deliberately the strict one, and it is deliberately not
// trusted in the other direction: `readOnlyHint` is a claim by the same server
// the call is about, exactly like a C5 attestation is a claim by the skill that
// made it. Believing it to *skip* a gate would let a server disarm the guard by
// lying. Believing it to *require* one costs an approval prompt on a tool that
// only reads, and `--trust-annotations` exists for operators who would rather
// have the prompts back than review them. See README, "Skill isolation", for
// the same argument about what a declaration is worth.
package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/mcpcli"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
)

// Upstream is one MCP server to front.
type Upstream struct {
	Name    string            `json:"-"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Disabled mirrors the field the desktop clients write, so a server the
	// operator already turned off there does not come back to life here.
	Disabled bool `json:"disabled,omitempty"`
}

func (u Upstream) transport() string {
	if u.URL != "" {
		return "http"
	}
	return "stdio"
}

// Config is the set of servers to front.
//
// The wire format is the one Claude Desktop, Cursor and the rest already write
// — a top-level `mcpServers` object. That is not laziness about designing a
// format: the fastest path to a guarded agent is pointing this at the config
// file the operator already has, and a bespoke schema would make "try it" mean
// "first translate your config".
type Config struct {
	Servers map[string]Upstream `json:"mcpServers"`
}

// ParseConfig reads the `mcpServers` document.
func ParseConfig(raw []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse mcp config: %w", err)
	}
	if len(c.Servers) == 0 {
		return nil, fmt.Errorf("no servers found: expected a top-level \"mcpServers\" object")
	}
	for name, u := range c.Servers {
		if u.Command == "" && u.URL == "" {
			return nil, fmt.Errorf("server %q declares neither \"command\" nor \"url\"", name)
		}
		u.Name = name
		c.Servers[name] = u
	}
	return &c, nil
}

// Options configure a run.
type Options struct {
	BaseURL string // http://127.0.0.1:9080
	Token   string
	Log     *slog.Logger
	// TrustAnnotations honours `readOnlyHint` to register a tool as sensorial.
	// Off by default; see the package comment for why believing a server about
	// its own safety is the direction not to be wrong in.
	TrustAnnotations bool
	// DryRun connects upstream and reports what would be registered without
	// registering anything — the same courtesy `aura connect` extends before
	// projecting an OpenAPI spec onto a live system.
	DryRun bool
}

// Guard fronts a set of MCP servers.
type Guard struct {
	opts Options
	cfg  *Config
	log  *slog.Logger

	mu     sync.Mutex
	bound  []Binding
	closer []func() error
}

// Binding is one upstream tool, as this node now sees it.
type Binding struct {
	Server     string
	Tool       string
	SkillID    string
	Capability string
	Type       string
	ReadOnly   bool
}

// Gated reports whether reaching this tool needs human approval under the
// node's default posture.
func (b Binding) Gated() bool { return b.Type == spec.TypeMotor }

func New(cfg *Config, opts Options) *Guard {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Guard{cfg: cfg, opts: opts, log: opts.Log}
}

// Bindings returns what was registered, for the CLI to print.
func (g *Guard) Bindings() []Binding {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Binding(nil), g.bound...)
}

var (
	idSeg  = regexp.MustCompile(`[^a-z0-9-]+`)
	capSeg = regexp.MustCompile(`[^a-z0-9_-]+`)
)

// slugID sanitizes into a C1 id segment (`[a-z0-9-]+`).
func slugID(s string) string {
	out := strings.Trim(idSeg.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		out = "x"
	}
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	return out
}

// slugCap sanitizes into a C1 capability segment, which also permits `_`.
func slugCap(s string) string {
	out := strings.Trim(capSeg.ReplaceAllString(strings.ToLower(s), "_"), "_-")
	if out == "" {
		out = "x"
	}
	if len(out) > 48 {
		out = strings.Trim(out[:48], "_-")
	}
	return out
}

// manifestFor builds the C1 manifest that makes one MCP tool a skill.
//
// `format: projection` rather than `source`: nothing about this tool runs on
// this node, and calling it `source` would claim a locality that is not true —
// the same reason `aura connect` types a projected OpenAPI operation that way.
// The ports are std/api-request@1 and std/api-response@1, unchanged from the
// OpenAPI projection, because "a call into a connected system" is exactly what
// this is and a second schema for the same shape helps nobody.
func manifestFor(server string, t mcpcli.Tool, trustAnnotations bool) registry.Manifest {
	readOnly := trustAnnotations && t.ReadOnly()
	skillType := spec.TypeMotor
	if readOnly {
		skillType = spec.TypeSensorial
	}
	srvID, toolID := slugID(server), slugID(t.Name)
	srvCap, toolCap := slugCap(server), slugCap(t.Name)

	desc := strings.TrimSpace(t.Description)
	if desc == "" {
		desc = fmt.Sprintf("Tool %q on MCP server %q.", t.Name, server)
	}
	// The gating posture belongs in the description because the description is
	// what the planner and the model actually read.
	if skillType == spec.TypeMotor {
		desc += " [acts on the world — every call is gated]"
	} else {
		desc += " [declared read-only by its server]"
	}

	m := registry.Manifest{
		ID:          fmt.Sprintf("mcp/%s/%s", srvID, toolID),
		Version:     "1.0.0",
		Protocol:    channel.ProtocolMajor,
		Name:        firstNonEmpty(t.Title, t.Name),
		Description: desc,
		Capability:  fmt.Sprintf("%s.mcp.%s.%s", skillType, srvCap, toolCap),
		Type:        skillType,
		Format:      "projection",
	}
	m.Ports.Ingress = []registry.Port{{Name: "call", Schema: "std/api-request@1"}}
	m.Ports.Egress = []registry.Port{{Name: "result", Schema: "std/api-response@1"}}
	return m
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "tool"
}

// Run connects every upstream, registers a skill per tool, and serves calls
// until ctx is cancelled.
func (g *Guard) Run(ctx context.Context) error {
	type conn struct {
		up     Upstream
		client *mcpcli.Client
		tools  []mcpcli.Tool
	}
	var conns []conn

	for name, up := range g.cfg.Servers {
		if up.Disabled {
			g.log.Info("guard: skipping disabled server", "server", name)
			continue
		}
		up.Name = name
		c, err := g.dial(ctx, up)
		if err != nil {
			// One unreachable server must not take the others down: a guard
			// that refuses to start because an optional tool server is missing
			// is a guard the operator turns off.
			g.log.Error("guard: upstream unreachable", "server", name, "err", err)
			continue
		}
		tools, err := c.ListTools(ctx)
		if err != nil {
			g.log.Error("guard: tools/list failed", "server", name, "err", err)
			_ = c.Close()
			continue
		}
		conns = append(conns, conn{up: up, client: c, tools: tools})
		g.mu.Lock()
		g.closer = append(g.closer, c.Close)
		g.mu.Unlock()
	}
	if len(conns) == 0 {
		return fmt.Errorf("no upstream MCP server could be reached")
	}

	for _, cn := range conns {
		for _, t := range cn.tools {
			m := manifestFor(cn.up.Name, t, g.opts.TrustAnnotations)
			if err := m.Validate(); err != nil {
				g.log.Error("guard: tool does not project to a valid skill",
					"server", cn.up.Name, "tool", t.Name, "err", err)
				continue
			}
			g.mu.Lock()
			g.bound = append(g.bound, Binding{
				Server: cn.up.Name, Tool: t.Name, SkillID: m.ID,
				Capability: m.Capability, Type: m.Type,
				ReadOnly: m.Type == spec.TypeSensorial,
			})
			g.mu.Unlock()
		}
	}
	if g.opts.DryRun {
		g.closeAll()
		return nil
	}

	var wg sync.WaitGroup
	for _, cn := range conns {
		for _, t := range cn.tools {
			m := manifestFor(cn.up.Name, t, g.opts.TrustAnnotations)
			if m.Validate() != nil {
				continue
			}
			wg.Add(1)
			go func(client *mcpcli.Client, tool mcpcli.Tool, man registry.Manifest) {
				defer wg.Done()
				g.serve(ctx, client, tool, man)
			}(cn.client, t, m)
		}
	}
	<-ctx.Done()
	wg.Wait()
	g.closeAll()
	return nil
}

func (g *Guard) closeAll() {
	g.mu.Lock()
	cs := g.closer
	g.closer = nil
	g.mu.Unlock()
	for _, c := range cs {
		_ = c()
	}
}

func (g *Guard) dial(ctx context.Context, up Upstream) (*mcpcli.Client, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if up.transport() == "http" {
		h := http.Header{}
		for k, v := range up.Headers {
			h.Set(k, v)
		}
		return mcpcli.DialHTTP(dialCtx, up.URL, h)
	}
	env := make([]string, 0, len(up.Env))
	for k, v := range up.Env {
		env = append(env, k+"="+v)
	}
	return mcpcli.DialStdio(dialCtx, up.Command, up.Args, env)
}

// serve keeps one tool registered, reconnecting if the socket drops.
//
// Reconnection matters more here than for an ordinary skill: a guard that
// silently stops fronting a tool after a blip leaves the agent calling an
// unguarded path, which is the one failure mode that must not be quiet. Losing
// the connection therefore retries forever, with backoff, and says so.
func (g *Guard) serve(ctx context.Context, client *mcpcli.Client, tool mcpcli.Tool, m registry.Manifest) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		err := g.session(ctx, client, tool, m)
		if ctx.Err() != nil {
			return
		}
		g.log.Warn("guard: skill connection lost, reconnecting",
			"skill", m.ID, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// session runs one registration for one tool.
func (g *Guard) session(ctx context.Context, client *mcpcli.Client, tool mcpcli.Tool, m registry.Manifest) error {
	wsURL := strings.Replace(g.opts.BaseURL, "http", "ws", 1) + "/ws/skill"
	hdr := http.Header{}
	if g.opts.Token != "" {
		hdr.Set("Authorization", "Bearer "+g.opts.Token)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer conn.Close()

	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := conn.WriteJSON(channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(),
		Kind: channel.KindRegister, Payload: manifestJSON,
	}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Writes are serialized: replies are produced by per-call goroutines so a
	// slow upstream tool does not head-of-line block the others, and a gorilla
	// connection tolerates exactly one concurrent writer.
	var writeMu sync.Mutex
	send := func(e channel.Envelope) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(e)
	}

	// A dropped context must unblock ReadJSON, which has no ctx form.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	var seq uint64
	var seqMu sync.Mutex
	for {
		var env channel.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return err
		}
		switch env.Kind {
		case channel.KindData:
			go func(in channel.Envelope) {
				seqMu.Lock()
				seq++
				n := seq
				seqMu.Unlock()
				out := g.invoke(ctx, client, tool, in, n)
				if err := send(out); err != nil {
					g.log.Error("guard: reply failed", "skill", m.ID, "err", err)
				}
			}(env)
		case channel.KindCancel:
			// Nothing to do beyond letting it drop: MCP has no cancellation for
			// an in-flight tools/call, and the kernel suppresses whatever this
			// chain would have produced anyway. Recorded, not pretended about.
			g.log.Info("guard: cancel received", "skill", m.ID, "cause", env.CauseID)
		}
	}
}

// invoke performs the upstream call for one delivered envelope.
//
// By the time an envelope arrives here the kernel has already decided the call
// may happen — policy consulted, gate approved if the capability is motor, the
// effect sealed. So this does the narrow thing: unwrap arguments, call, wrap
// the answer as std/api-response@1.
func (g *Guard) invoke(ctx context.Context, client *mcpcli.Client, tool mcpcli.Tool,
	in channel.Envelope, seq uint64) channel.Envelope {

	reply := func(payload any) channel.Envelope {
		raw, _ := json.Marshal(payload)
		return channel.Envelope{
			V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: in.ID,
			Session: in.Session, Node: in.Node, Port: "result", Seq: seq,
			Idem:    fmt.Sprintf("%s:%s:result:%d", in.Idem, in.Node, seq),
			Schema:  "std/api-response@1",
			Kind:    channel.KindData,
			Payload: raw,
		}
	}

	args, err := arguments(in.Payload)
	if err != nil {
		return reply(map[string]any{"ok": false, "error": err.Error()})
	}

	callCtx := ctx
	if in.Deadline > 0 {
		// The edge's deadline is honoured rather than ignored: an answer that
		// arrives after nobody is listening costs an upstream side effect for
		// nothing.
		var cancel context.CancelFunc
		callCtx, cancel = context.WithDeadline(ctx, time.UnixMilli(in.Deadline))
		defer cancel()
	}

	res, err := client.CallTool(callCtx, tool.Name, args)
	if err != nil {
		return reply(map[string]any{"ok": false, "error": err.Error()})
	}
	out := map[string]any{"ok": !res.IsError}
	if txt := res.Text(); txt != "" {
		out["body"] = txt
	}
	if len(res.StructuredContent) > 0 {
		out["body"] = res.StructuredContent
	}
	if res.IsError {
		out["error"] = firstNonEmpty(res.Text(), "the tool reported an error")
	}
	return reply(out)
}

// arguments pulls the tool arguments out of a std/api-request@1 payload.
//
// `body` is the declared home for a call's content, so it wins. A payload that
// carries neither `body` nor `params` is passed through whole, which is what
// makes a hand-written graph that just sends `{"path": "/tmp"}` work without
// the author having to learn the envelope shape first.
func arguments(payload []byte) (map[string]any, error) {
	if len(payload) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object")
	}
	if body, ok := doc["body"]; ok {
		if m, ok := body.(map[string]any); ok {
			return m, nil
		}
		return nil, fmt.Errorf("\"body\" must be an object of tool arguments")
	}
	if params, ok := doc["params"]; ok {
		if m, ok := params.(map[string]any); ok {
			return m, nil
		}
	}
	delete(doc, "query")
	delete(doc, "headers")
	return doc, nil
}
