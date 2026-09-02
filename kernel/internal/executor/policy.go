package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"aura/kernel/internal/ledger"
)

// Node-level authorization policy.
//
// Who gets to decide that an effect needs no approval — that is the real
// problem here. Before this file the answer was "whoever wrote the graph": an
// edge carrying `"gate": "none"` waived the motor-gate invariant outright, and a
// graph is nothing more than a JSON document anyone can POST to /v1/graphs. So
// the kernel's strongest guarantee was worth nothing against anyone who could
// register a graph — and before authentication landed in that same release,
// that was literally anyone who could reach the port.
//
// Authorization now lives in one document, loaded at startup, outside every
// graph, and hashed — so an auditor can prove which one was in force. A graph
// may ask for less friction, but the node grants or refuses that request; the
// graph never decides it.
//
// Deliberately NOT a rules language with expressions. A policy an auditor
// cannot read at a glance has stopped being a policy, so this is: an ordered
// list, first match wins, three possible decisions, an optional rate limit. No
// callbacks, and no evaluation order anyone has to reconstruct in their head.

// Decision is what the node has decided about an effect. Read them as ordered
// — allow < gate < deny — because the whole rule this file implements is that
// a graph may move an effect *up* that order and never down without
// permission. Authorize spells the consequence out.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionGate  Decision = "gate"
	DecisionDeny  Decision = "deny"
)

func (d Decision) valid() bool {
	return d == DecisionAllow || d == DecisionGate || d == DecisionDeny
}

// Limit caps how often a capability may act. Zero means unlimited.
type Limit struct {
	PerMinute int `yaml:"per_minute"`
}

// Speculation is a node's stance on running work ahead of certainty (C2 v1.2).
//
// It is a policy question and not only a graph one for the same reason gates
// are: speculation spends compute and, on a shared node, one graph's guessing
// is another graph's queue. An operator who wants none can say so once, in the
// document an auditor already reads, rather than reviewing every graph.
type Speculation string

const (
	// SpeculationAllow honours an edge that asks to speculate. The motor
	// refusal still applies — no policy can waive that.
	SpeculationAllow Speculation = "allow"
	// SpeculationDeny ignores every speculative request on this node.
	SpeculationDeny Speculation = "deny"
)

// SealFailure is a node's stance on an effect it could not seal (C4).
//
// Sealing writes to disk, and disks fill up and break. The question this answers
// is which of two bad outcomes an operator prefers when that happens on the
// delivery path:
//
//   - `deliver` keeps the node running and leaves a documented hole in the
//     ledger. The effect happened; the record of it did not. The error is
//     logged loudly, but nothing downstream is stopped.
//   - `refuse` keeps the ledger complete and stops the effect instead. Nothing
//     acts on the world that this node cannot afterwards prove it authorized.
//
// There is no third option where both hold, so it is exposed rather than
// decided here. `deliver` is the default because a node whose premise is that
// it never stops running should not turn a full disk into an outage — but a
// node sealing payments wants `refuse`, and until this existed it could not
// have it. What the default must never do is be quiet about which one is in
// force: the startup banner prints it, and the guide says plainly that the
// ledger's completeness under `deliver` is best-effort.
type SealFailure string

const (
	// SealFailureDeliver lets the effect through and logs the gap.
	SealFailureDeliver SealFailure = "deliver"
	// SealFailureRefuse stops the effect rather than leave it unattested.
	SealFailureRefuse SealFailure = "refuse"
)

func (s SealFailure) valid() bool {
	return s == "" || s == SealFailureDeliver || s == SealFailureRefuse
}

// Route pins which package answers a capability, in preference order.
//
// Model choice is a governance question, not only an engineering one: "which
// model answered this" is a thing an auditor asks, and the honest answer
// should be readable in the same signed document that says what may act on the
// world. Every other runtime settles this in application code or in a router
// service, where it is invisible to the person reviewing the deployment.
//
// This is deliberately *not* a learned router. RouteLLM-style classifiers and
// the vLLM semantic router pick per query and are genuinely better at cost and
// quality; they are also unauditable by inspection, which is the property this
// runtime trades for. A node that wants a learned router puts one behind a
// `cognitive.*` capability and routes to it here — the two compose.
type Route struct {
	// Match is a capability, an exact one or a `prefix.*` wildcard, using the
	// same matcher as Rule.
	Match string `yaml:"match"`
	// Prefer lists package ids (`org/cat/name`) in descending preference. The
	// first one currently connected wins. A capability whose preferred
	// packages are all offline falls through to ordinary resolution rather
	// than failing — a routing preference should not take a graph down.
	Prefer []string `yaml:"prefer,omitempty"`
	// Avoid lists package ids never chosen for this capability unless nothing
	// else provides it. Same reasoning: a preference, not a prohibition.
	// Use a `deny` rule to actually forbid something.
	Avoid  []string `yaml:"avoid,omitempty"`
	Reason string   `yaml:"reason,omitempty"`
}

// Rule is one line of policy. Match is an exact capability, a prefix wildcard
// (`motor.erp.*`), or `*`.
type Rule struct {
	Match    string   `yaml:"match"`
	Decision Decision `yaml:"decision"`
	Reason   string   `yaml:"reason,omitempty"`
	Limit    *Limit   `yaml:"limit,omitempty"`
}

// Policy is the whole document.
type Policy struct {
	Version int      `yaml:"policy"`
	Default Decision `yaml:"default_effect"`
	// AllowGraphWaiver decides whether an edge's `"gate": "none"` is honoured.
	// A pointer so "absent" is distinguishable from "false": absent in a file
	// means false (a node given a policy is a node that wants to be the
	// authority), while the built-in default policy sets it true so that
	// `aura up` with no policy behaves exactly as it did before this existed.
	AllowGraphWaiver *bool `yaml:"allow_graph_waiver,omitempty"`
	// Speculation decides whether edges asking to run ahead of certainty are
	// honoured. Empty means allow, which is the pre-v1.2 behaviour for graphs
	// that never ask — a graph with no speculative edge is unaffected either
	// way, so defaulting to allow costs nothing and keeps the flag opt-out.
	Speculation Speculation `yaml:"speculation,omitempty"`
	// RequireSignedApproval turns the human-approval gate from "somebody
	// clicked yes" into "this enrolled person signed yes" (C4 v1.3).
	//
	// Off by default, and that default is a real decision rather than
	// timidity: a node with nobody enrolled would refuse every gated effect,
	// so switching this on silently at upgrade would break running nodes on
	// their first write. It is opt-in, it is refused at startup when the
	// roster is empty, and once on, an unsigned answer to a gate is a denial —
	// not a warning, since a gate that degrades to trusting whoever holds the
	// socket is the gate this flag exists to replace.
	RequireSignedApproval bool `yaml:"require_signed_approval,omitempty"`
	// RequireApprovalContext names the labels an approval must bind before it
	// releases an effect (C4 v1.7) — "screen", "invoice", "diff", whatever the
	// artifact is that this deployment says a person has to have in front of
	// them. An answer whose signature does not cover every label listed here is
	// a denial.
	//
	// This is the difference between "somebody with the right key said yes" and
	// "somebody with the right key said yes to *this document*". Without it, a
	// surface that renders a reassuring summary over an effect that does
	// something else produces an approval no auditor can tell from an honest
	// one, because there is nothing in the record about what was rendered.
	//
	// Labels and not a count. Requiring "at least one artifact" is satisfied by
	// binding any artifact, including a constant, so it would be an enforcement
	// that enforces nothing — the same failure this contract already refuses on
	// the unsigned path.
	//
	// Setting this implies RequireSignedApproval, because a context lives inside
	// the operator's signature and an unsigned answer therefore cannot carry
	// one. A node that asked for bound approvals and kept accepting unsigned
	// answers would be asking for nothing at all.
	RequireApprovalContext []string `yaml:"require_approval_context,omitempty"`
	// OnSealFailure decides what happens to an effect the ledger could not seal.
	// Empty means SealFailureDeliver. See SealFailure.
	OnSealFailure SealFailure `yaml:"on_seal_failure,omitempty"`
	Rules         []Rule      `yaml:"rules,omitempty"`
	// Routes pin a capability to specific packages, in preference order.
	Routes []Route `yaml:"routes,omitempty"`

	source string // human-readable origin, for logs and errors
	hash   string // sha256 of the exact document in force

	mu      sync.Mutex
	buckets map[string]*bucket
}

// DefaultPolicy is what a node runs with when no --policy file is given.
//
// It reproduces the behaviour that existed before policy did: motor edges are
// gated unless the graph waives them. That keeps `aura up` working out of the
// box — the seeded `voice` graph waives the gate on speech, and a voice
// assistant that asked permission before every clause would not be one.
//
// It is a real document rather than an implicit code path: it has a hash, it
// is printed at startup, and it can be dumped. An implicit default is one
// nobody can audit.
func DefaultPolicy() *Policy {
	waiver := true
	p := &Policy{
		Version:          1,
		Default:          DecisionGate,
		AllowGraphWaiver: &waiver,
		source:           "built-in default (no --policy given)",
	}
	p.finalize(nil)
	return p
}

// validateRequiredContext checks that a configured list of labels is one an
// approval could actually carry, by running it past the same function that will
// judge the real thing. A separate copy of the rules here is how the two come
// to disagree, and the disagreement would surface as a node that denies every
// gated effect for reasons its logs blame on the client.
func validateRequiredContext(labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	// The digest is a placeholder: only the labels are under test, and every
	// entry needs a well-formed one to get that far.
	const wellFormed = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	entries := make([]ledger.ContextEntry, 0, len(labels))
	for _, label := range labels {
		entries = append(entries, ledger.ContextEntry{Label: label, Digest: wellFormed})
	}
	_, err := ledger.CanonicalContext(entries)
	return err
}

// LoadPolicy reads a policy document. An empty path yields DefaultPolicy.
func LoadPolicy(path string) (*Policy, error) {
	if path == "" {
		return DefaultPolicy(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy %q: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse policy %q: %w", path, err)
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("policy %q: unsupported policy version %d (this kernel speaks 1)",
			path, p.Version)
	}
	if p.Default == "" {
		p.Default = DecisionGate
	}
	if !p.Default.valid() {
		return nil, fmt.Errorf("policy %q: default_effect %q must be allow|gate|deny", path, p.Default)
	}
	if !p.OnSealFailure.valid() {
		return nil, fmt.Errorf("policy %q: on_seal_failure %q must be deliver|refuse", path, p.OnSealFailure)
	}
	for i, r := range p.Rules {
		if strings.TrimSpace(r.Match) == "" {
			return nil, fmt.Errorf("policy %q: rule %d has an empty match", path, i+1)
		}
		if !r.Decision.valid() {
			return nil, fmt.Errorf("policy %q: rule %d (%s): decision %q must be allow|gate|deny",
				path, i+1, r.Match, r.Decision)
		}
		if r.Limit != nil && r.Limit.PerMinute < 0 {
			return nil, fmt.Errorf("policy %q: rule %d (%s): per_minute cannot be negative",
				path, i+1, r.Match)
		}
	}
	// A requirement nothing could satisfy would deny every gated effect on this
	// node, and the operator would read the refusal as a broken client. It is
	// caught here, where the file that caused it can be named, and it is caught
	// by the validator a real approval goes through rather than by a second copy
	// of the rules — a bad label, a repeated one and a list longer than an
	// approval may carry all come back from that one call.
	if err := validateRequiredContext(p.RequireApprovalContext); err != nil {
		return nil, fmt.Errorf("policy %q: require_approval_context: %w", path, err)
	}
	// Absent in a file means false: a node handed a policy is a node that
	// wants to be the authority on what may act.
	if p.AllowGraphWaiver == nil {
		no := false
		p.AllowGraphWaiver = &no
	}
	p.source = path
	p.finalize(raw)
	return &p, nil
}

// finalize computes the document hash and prepares rate-limit state. The hash
// is over the exact bytes when they came from a file, so it identifies the
// document an auditor can go and read; for the built-in default there are no
// bytes, so it is over a canonical rendering of the struct.
func (p *Policy) finalize(raw []byte) {
	if raw == nil {
		raw, _ = yaml.Marshal(p)
	}
	sum := sha256.Sum256(raw)
	p.hash = "sha256:" + hex.EncodeToString(sum[:])
	p.buckets = map[string]*bucket{}
}

// Hash identifies the document in force. It goes in the startup banner now and
// in every ledger entry once the ledger lands, so "which policy allowed this"
// is answerable after the fact.
func (p *Policy) Hash() string { return p.hash }

// Source is where the policy came from, for logs and error messages.
func (p *Policy) Source() string { return p.source }

// GraphWaiverAllowed reports whether an edge may waive the gate itself.
func (p *Policy) GraphWaiverAllowed() bool {
	return p.AllowGraphWaiver != nil && *p.AllowGraphWaiver
}

// SpeculationAllowed reports whether this node honours speculative edges.
func (p *Policy) SpeculationAllowed() bool {
	return p.Speculation != SpeculationDeny
}

// SignedApprovalRequired reports whether a gate may only be answered by an
// enrolled operator's signature (C4 v1.3).
//
// True when the policy asks for a context as well, since a context is carried
// inside the signature: requiring one while accepting unsigned answers would
// leave the unsigned path as the cheaper route around it.
func (p *Policy) SignedApprovalRequired() bool {
	return p.RequireSignedApproval || len(p.RequireApprovalContext) > 0
}

// ApprovalContextRequired lists the labels an approval must bind (C4 v1.7).
// Empty means an approval may bind anything, or nothing.
func (p *Policy) ApprovalContextRequired() []string { return p.RequireApprovalContext }

// SealFailureRefuses reports whether an effect that could not be sealed must be
// stopped rather than delivered unattested. See SealFailure.
func (p *Policy) SealFailureRefuses() bool { return p.OnSealFailure == SealFailureRefuse }

// SealFailureStance is what the startup banner prints, so that which of the two
// bad outcomes this node prefers is visible without reading the policy file.
func (p *Policy) SealFailureStance() SealFailure {
	if p.OnSealFailure == "" {
		return SealFailureDeliver
	}
	return p.OnSealFailure
}

// RouteFor returns the node's package preferences for a capability: an
// ordered `prefer` list and a set to avoid. First matching route wins, like
// every other ordered list in this document.
func (p *Policy) RouteFor(capability string) (prefer []string, avoid map[string]bool) {
	for _, r := range p.Routes {
		if !matchCapability(r.Match, capability) {
			continue
		}
		if len(r.Avoid) > 0 {
			avoid = make(map[string]bool, len(r.Avoid))
			for _, id := range r.Avoid {
				avoid[id] = true
			}
		}
		return r.Prefer, avoid
	}
	return nil, nil
}

// Decide resolves what the node has decided about one capability, returning
// the decision, the reason (for the audit trail and for the message a denied
// author sees), and the matched rule's limit if it had one.
//
// Rules are ordered and the first match wins. With no matching rule, an effect
// — anything a `motor.*` skill does — falls to default_effect, while anything
// else is allowed: the default is about *acting on the world*, and applying it
// to a parser or an LLM would gate every graph into uselessness.
func (p *Policy) Decide(capability string) (Decision, string, *Limit) {
	for _, r := range p.Rules {
		if matchCapability(r.Match, capability) {
			reason := r.Reason
			if reason == "" {
				reason = fmt.Sprintf("policy rule %q", r.Match)
			}
			return r.Decision, reason, r.Limit
		}
	}
	if isEffect(capability) {
		return p.Default, "policy default_effect", nil
	}
	return DecisionAllow, "not an effect", nil
}

// isEffect reports whether a capability acts on the world. The capability
// taxonomy is `<type>.<function>...` (C1 rule 4), so the type prefix is the
// authority — and it is the same prefix the manifest's declared type must
// match, which registry.Manifest.Validate enforces.
func isEffect(capability string) bool {
	return capability == TypeMotor || strings.HasPrefix(capability, TypeMotor+".")
}

// matchCapability implements the three match forms: `*`, a `prefix.*`
// wildcard, and an exact capability. A prefix wildcard also matches the bare
// prefix, so `motor.erp.*` covers `motor.erp` itself — otherwise a rule would
// silently miss the capability its author most obviously meant.
func matchCapability(match, capability string) bool {
	if match == "*" {
		return true
	}
	if strings.HasSuffix(match, ".*") {
		prefix := strings.TrimSuffix(match, ".*")
		return capability == prefix || strings.HasPrefix(capability, prefix+".")
	}
	return match == capability
}

// Authorization is what the node decided about one edge.
type Authorization struct {
	// Gate is the gate to apply: "" or GateHumanApproval.
	Gate string
	// Waived records that policy wanted a gate here and the *graph* is the
	// reason there is not one.
	//
	// It is tracked separately from `Gate == ""` because the two are not the
	// same fact, and the difference is the whole of who authorized the effect.
	// Policy saying `allow` is the node deciding an effect needs no supervision.
	// A graph saying `gate: none` under a policy that permits waivers is
	// whoever registered that graph deciding it — and a graph is a JSON document
	// anyone holding the node token can POST. The ledger records the
	// distinction (Entry.Waived) and the credential broker refuses to spend a
	// receipt carrying it, so a waiver can lower friction without also becoming
	// a way to mint the authorization that buys a credential.
	Waived bool
}

// Authorize is the runtime check: it combines the node's decision with what
// the graph asked for and returns the gate to apply, or an error if the effect
// is refused outright.
//
// The rule in one line: **a graph may be stricter than policy, never laxer.**
//
//   - policy deny                     → refused, whatever the graph says
//   - policy gate, graph `none`       → gated (this is the hole being closed)
//   - policy allow, graph `none`      → delivered, if waivers are permitted
//   - policy allow, graph human-approval → gated (the author asked for more)
//
// graphGate is the edge's declared gate: "", GateNone or GateHumanApproval.
//
// This is the *static* half of the check, run once when a session is wired, so
// a denied graph never starts rather than failing on its first message. The
// dynamic half — a rule's rate limit, which is inherently per-delivery — is
// CheckRate, called from the forwarding path.
func (p *Policy) Authorize(capability, graphGate string) (Authorization, error) {
	decision, reason, _ := p.Decide(capability)

	// A deny is absolute and is checked first, so nothing below can talk its
	// way past it.
	if decision == DecisionDeny {
		return Authorization{}, fmt.Errorf("policy denies %s (%s); no graph may override a deny",
			capability, reason)
	}

	// Raising strictness needs no permission: an author who writes the gate
	// down has asked for more supervision than they were required to have, and
	// there is never a reason to refuse them that.
	if graphGate == GateHumanApproval {
		return Authorization{Gate: GateHumanApproval}, nil
	}

	// Lowering it does. A waiver is a *request*, granted only by a node that
	// said graphs may make this call. Where it is not granted the request is
	// ignored rather than rejected — the graph is not malformed, it simply does
	// not get to decide this.
	if graphGate == GateNone && p.GraphWaiverAllowed() {
		// Only a waiver that actually removed a gate is a waiver. Under a policy
		// that already said `allow`, `gate: none` asked for nothing it was not
		// getting, and recording it as an escalation would make the ledger cry
		// wolf on every voice graph.
		return Authorization{Waived: decision == DecisionGate}, nil
	}

	if decision == DecisionGate {
		return Authorization{Gate: GateHumanApproval}, nil
	}
	return Authorization{}, nil
}

// CheckRate is the dynamic half of the check, called on every delivery into a
// skill. A limit cannot be resolved when the session is wired, because the
// question it answers — "how many times has this already happened this
// minute" — only exists at delivery time.
//
// Capabilities without a limit cost one walk of an ordered list of a handful
// of rules, so this stays on the hot path without a cache to keep stale.
func (p *Policy) CheckRate(capability string) error {
	_, reason, limit := p.Decide(capability)
	if limit == nil || limit.PerMinute <= 0 {
		return nil
	}
	if !p.allowRate(capability, limit.PerMinute) {
		return fmt.Errorf("policy rate limit for %s exceeded (%d/min, %s)",
			capability, limit.PerMinute, reason)
	}
	return nil
}

// bucket is a fixed-window counter. A sliding window would be more precise and
// is not worth the memory here: policy limits exist to stop a runaway agent
// hammering an ERP, not to meter billing.
type bucket struct {
	windowStart time.Time
	count       int
}

func (p *Policy) allowRate(capability string, perMinute int) bool {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.buckets[capability]
	if !ok {
		// Bound the map: a node whose capabilities churn without limit must not
		// grow this forever. Capabilities are few, so a high cap never bites.
		if len(p.buckets) > 1024 {
			p.buckets = map[string]*bucket{}
		}
		b = &bucket{windowStart: now}
		p.buckets[capability] = b
	}
	if now.Sub(b.windowStart) >= time.Minute {
		b.windowStart = now
		b.count = 0
	}
	if b.count >= perMinute {
		return false
	}
	b.count++
	return true
}
