package gateway

// Scoped credentials — who a caller is, and what that lets them do.
//
// # The hole this closes
//
// A node had exactly one credential. The CLI, the browser UI, a federation
// bridge and every skill process presented the same bearer token, so "can reach
// this node" and "is the operator of this node" were the same statement. That
// made two guarantees weaker than they read:
//
//   - **The credential broker.** Its argument is that skipping the Effect
//     Checkpoint gets you a 401, because the secret is released only against the
//     receipt of an effect that just passed the gate. True against a process
//     outside the node — and not against a skill, which held the operator's
//     token and could therefore register a graph waiving its own gate, seal an
//     effect, and redeem the receipt it had just minted. The `waived` flag now
//     refuses that receipt (C4 v1.5), but the deeper problem was that a skill
//     could register the graph at all.
//   - **The approval gate.** A compromised skill holding the operator token
//     could register a second skill under any capability it liked, including one
//     the policy allows outright.
//
// A skill needs three things and nothing else: to connect, to register as the
// one capability it was issued for, and to redeem receipts it was handed. That
// is what a `skill` token can do here, and the binding at registration is the
// load-bearing half — a narrow token that could still register as any capability
// would be no narrower at all.
//
// # Why a table rather than checks at each route
//
// The same argument the policy engine makes about rules: an authorization model
// an auditor cannot read at a glance is not one. Scattering `if principal.Kind
// != operator` across forty handlers produces a model that exists only as the
// sum of forty decisions, and a route added later silently defaults to whatever
// its author remembered. So there is one ordered table, read top to bottom,
// first match wins, and everything not matched requires the operator.
//
// Deny-by-default in the same sense the policy is: a new route is closed to
// skills until someone adds a line here saying otherwise, and that line is a
// diff a reviewer can see.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// errNoCredential is every authentication failure. One error for all of them
// deliberately: a caller must not be able to tell "no token" from "unknown
// token" from "revoked token", because the difference is an oracle.
var errNoCredential = errors.New("no valid credential")

// principalKey types the context value, so nothing else can collide with it or
// read it by accident.
type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalOf returns the authenticated caller behind a request.
//
// The zero value it falls back to is `Scope("")`, which `Allows` refuses and
// `MayRegister` refuses — so a handler reached by a path that somehow bypassed
// authentication is denied rather than treated as the operator. Fail-closed is
// the only sane default for a function whose answer is "who is this".
func PrincipalOf(r *http.Request) Principal {
	if p, ok := r.Context().Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{}
}

// Scope is what a credential is for.
type Scope string

const (
	// ScopeOperator is the node's own bearer token: everything. This is what
	// `node.token` in the data directory has always been, and it keeps that
	// meaning so an existing deployment is unaffected.
	ScopeOperator Scope = "operator"
	// ScopeSkill is a credential issued to one skill process, bound to the one
	// capability it may register as.
	ScopeSkill Scope = "skill"
)

// Valid reports whether s is a scope this kernel issues.
func (s Scope) Valid() bool { return s == ScopeOperator || s == ScopeSkill }

// Principal is the authenticated caller.
type Principal struct {
	Scope Scope
	// TokenID identifies the credential for revocation and for the audit trail.
	// Empty for the operator token, which is a file rather than a row.
	TokenID string
	// Capability is the C1 capability a ScopeSkill principal may register as.
	// Empty for the operator, who may register anything.
	Capability string
	Label      string
}

// IsOperator reports whether this principal holds the node's own authority.
func (p Principal) IsOperator() bool { return p.Scope == ScopeOperator }

// MayRegister reports whether this principal may register a skill declaring
// `capability`.
//
// This is the check that makes a narrow token narrow. Without it a `skill`
// token would be a full one wearing a different name: registration is how a
// process decides what it *is* to the executor, so a caller free to choose that
// is a caller free to choose which policy rule applies to it.
//
// Exact match, never a prefix. `motor.erp.*` as an issued capability would let
// a token for `motor.erp.read` register as `motor.erp.delete`, and the whole
// point is that the operator decided which one at issue time.
func (p Principal) MayRegister(capability string) bool {
	if p.IsOperator() {
		return true
	}
	return p.Scope == ScopeSkill && p.Capability != "" && p.Capability == capability
}

// skillRoutes is the authority on what a `skill` credential may reach. Ordered,
// first match wins, everything else needs the operator.
//
// Each entry is a real need with a reason, and the list is short on purpose —
// a skill that needs more than this is asking to be the operator, and should be
// given that token deliberately rather than by widening this table.
var skillRoutes = []struct {
	method string // "" matches any
	path   string
	prefix bool
	why    string
}{
	// The skill socket itself. Registration is bound to the token's capability
	// inside the handshake — see Principal.MayRegister.
	{method: http.MethodGet, path: "/ws/skill", why: "the skill's own connection"},
	// Redeeming a receipt for the credentials that effect authorized. This is
	// the reason the broker exists and the one call a skill must be able to
	// make; it is safe here because the receipt, not the token, is what
	// authorizes it — see internal/broker.
	{method: http.MethodPost, path: "/v1/secrets/resolve", why: "spend a receipt for its credentials"},
	// Liveness, so a supervisor running as the skill can tell whether the node
	// is up without holding the operator's credential.
	{method: http.MethodGet, path: "/healthz", why: "liveness"},
	// A skill's own runtime config, hot-applied over its connection. Reading it
	// discloses the skill's own declared parameters and nothing about the node.
	{method: http.MethodGet, path: "/v1/skills/config/", prefix: true, why: "its own declared config"},
}

// Allows reports whether a principal may make this request.
func (p Principal) Allows(method, path string) bool {
	if p.IsOperator() {
		return true
	}
	if p.Scope != ScopeSkill {
		return false
	}
	for _, r := range skillRoutes {
		if r.method != "" && r.method != method {
			continue
		}
		if r.prefix {
			if strings.HasPrefix(path, r.path) {
				return true
			}
			continue
		}
		if path == r.path {
			return true
		}
	}
	return false
}

// HashToken is how a presented credential is matched against storage.
//
// Tokens are stored hashed for the same reason passwords are: a database file,
// a backup or a support dump should not be a set of live credentials. SHA-256
// with no salt is right here and would not be for passwords — these are 256 bits
// of `crypto/rand`, so there is no dictionary to run and no work factor to buy.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
