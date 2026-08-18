package gateway

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Node authentication.
//
// Before this file none of this existed: no token, no TLS, no origin check, and
// it listened on every network interface. Anyone who could reach the port could
// register a graph and run it — which left every other guarantee the kernel
// makes conditional on "nobody else being on the network", which is not a
// guarantee, it is wishful thinking.
//
// Three things close that gap, and they are kept separate deliberately, because
// each answers a different question:
//
//   - a bearer token, generated on first start, which answers "may this caller
//     act at all";
//   - an Origin allowlist on the WebSocket upgrade, which answers "may this
//     *web page* act on the user's behalf" — a different question, because a
//     browser attaches credentials to cross-site requests automatically and
//     the token alone would not stop a malicious page driving a local node;
//   - loopback binding by default, which shrinks who can ask in the first
//     place.
//
// None of them is sufficient alone, which is why the default posture uses all
// three and every relaxation is an explicit flag.

// tokenFile is where the node's bearer token lives inside the data dir.
const tokenFile = "node.token"

// TokenResolver looks up a scoped credential by its hash. Satisfied by
// *store.Store; an interface so that auth does not depend on the whole store,
// and so a test can present a roster without one.
type TokenResolver interface {
	TokenByHash(hash string) (row TokenRow, found bool, err error)
}

// TokenRow is what the resolver returns. It mirrors store.TokenRow rather than
// importing it, so the dependency runs one way: the gateway defines what it
// needs to authenticate, and storage satisfies it.
type TokenRow struct {
	ID         string
	Scope      string
	Capability string
	Label      string
	Revoked    int64
}

// Auth holds the node's access configuration.
type Auth struct {
	// Token is the operator's bearer token — the node's own authority, read
	// from `node.token` in the data directory. Empty disables authentication
	// entirely — only reachable via --no-auth, which exists for a single-user
	// loopback node and prints a warning.
	Token string
	// Tokens resolves scoped credentials issued with `aura token issue`. Nil
	// means only the operator token is accepted, which is what a node that has
	// issued none behaves like anyway.
	Tokens TokenResolver
	// AllowedOrigins is the exact-match allowlist for the WebSocket Origin
	// header. Empty means "same-origin only": a browser page served from
	// somewhere else is refused.
	AllowedOrigins []string
	// TrustedProxy relaxes the origin check for deployments that terminate TLS
	// upstream. Off by default.
	TrustedProxy bool
	// OpenWitness exempts the witnessing endpoints from the bearer token, so
	// nodes outside this trust domain can anchor their ledgers here. Set by
	// --open-witness. Off by default, because it turns a read-mostly control
	// surface into one with an unauthenticated write path — bounded by
	// ledger.WitnessLimits, but a deliberate decision either way.
	OpenWitness bool
}

// LoadOrCreateToken returns the node's bearer token, generating and persisting
// one on first start. 0600 because it is a credential; a token readable by
// every account on the machine is not one.
func LoadOrCreateToken(dataDir string) (token string, created bool, err error) {
	path := filepath.Join(dataDir, tokenFile)
	if raw, readErr := os.ReadFile(path); readErr == nil {
		if t := strings.TrimSpace(string(raw)); t != "" {
			return t, false, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("generate node token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", false, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("persist node token: %w", err)
	}
	return token, true, nil
}

// openPath reports whether a path is reachable without a node token. Three
// things are, each for a different and specific reason:
//
//   - /healthz — an orchestrator has to be able to ask whether a process is up
//     without holding a credential, and the answer leaks nothing an attacker
//     could not learn by watching the port accept connections.
//   - /hooks/* — the inbound webhook surface. Stripe and GitHub will never
//     hold a node token; these requests authenticate with their own HMAC over
//     a signed timestamp, which is a stronger check than a shared bearer.
//   - /v1/ledger/witness and its last-seen probe, ONLY when the node was
//     started with --open-witness. A witness that only serves parties who
//     already exchanged a bearer token is a witness inside the same trust
//     domain as the log it vouches for, which is most of what makes
//     Certificate Transparency's model work. The statement is
//     self-authenticating (it carries the presenting node's key and a
//     signature over the head), and the rate, capacity and retention bounds
//     in ledger.WitnessLimits are what keep an open write surface from being
//     a free database. Off by default.
//   - the UI's static assets — a browser must be able to load the page before
//     it can present a token. The assets contain no data; everything the page
//     then *asks for* goes through /v1/* and is authenticated.
//
// Note what is deliberately absent: /v1/skills and the A2A card. The
// capability catalog tells an attacker exactly which effects this node can
// produce, which is the most useful thing they could learn.
func openPath(p string, openWitness bool) bool {
	switch {
	case p == "/healthz":
		return true
	case strings.HasPrefix(p, "/hooks/"):
		return true
	case isUIAsset(p):
		return true
	case openWitness && p == "/v1/ledger/witness":
		return true
	case openWitness && p == "/v1/ledger/witness/last-seen":
		return true
	case openWitness && publicWitnessLogPath(p):
		// The witness's own log (C4 v1.4). Read-only, and public for the same
		// reason the witness is open at all: an anchor several mutually
		// suspicious parties rely on has to be one none of them is obliged to
		// trust, and that means all of them can read its history. These records
		// are heads, roots, keys and signatures — there is no payload, session
		// or capability in them to leak.
		return true
	}
	return false
}

// publicWitnessLogPath matches only the routes that publish *this* witness's
// own history.
//
// Enumerated rather than matched on the `/v1/witness/` prefix, and the
// difference is not cosmetic: `/v1/witness/seen` lives under the same prefix
// and is this node's private record of what it has verified about *other*
// witnesses. Opening that would let anyone lower the baseline an audit compares
// against — erasing, from outside, the evidence that would convict a witness of
// rewriting its log. A prefix match would have done exactly that.
func publicWitnessLogPath(p string) bool {
	switch p {
	case "/v1/witness/head", "/v1/witness/log", "/v1/witness/consistency":
		return true
	}
	return strings.HasPrefix(p, "/v1/witness/proof/")
}

// isUIAsset matches the embedded single-page app: its root and its bundled
// files. Anything under a versioned API prefix is never an asset, so a new
// /v1 route cannot be mistaken for one.
func isUIAsset(p string) bool {
	if strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/ws/") ||
		p == "/mcp" || p == "/.well-known/agent.json" {
		return false
	}
	return p == "/" || !strings.Contains(strings.TrimPrefix(p, "/"), "/")
}

// Authenticate wraps a handler with bearer-token enforcement.
//
// The token may arrive as `Authorization: Bearer <t>` or, for the WebSocket
// paths, as `?token=<t>` — the browser WebSocket API cannot set headers, so
// refusing the query form would mean no browser client could ever connect. The
// query form is accepted only on the WS upgrade paths, so a token cannot end
// up in an HTTP access log for ordinary API traffic.
func (a *Auth) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Token == "" {
			// An unauthenticated node has no principals to distinguish, so
			// everything runs as the operator — which is exactly what --no-auth
			// means and what its warning says.
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(),
				Principal{Scope: ScopeOperator})))
			return
		}
		if openPath(r.URL.Path, a.OpenWitness) {
			// An open path serves callers holding nothing, so it cannot demand a
			// credential — but it must not *launder* one either. Resolving here
			// keeps a skill token a skill token; skipping resolution would hand
			// the handler an operator principal for any request that happened to
			// land on a public route, which is a privilege escalation waiting for
			// the first open path whose behaviour depends on who is asking.
			//
			// A request with no credential gets the zero principal — nobody —
			// which every scope check refuses.
			p, err := a.resolve(r)
			if err != nil {
				p = Principal{}
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
			return
		}
		p, err := a.resolve(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="aura"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "missing or invalid bearer token — the operator token is in <data-dir>/" +
					tokenFile + "; a skill token comes from `aura token issue`",
			})
			return
		}
		// Scope is enforced here rather than in each handler, so a route added
		// later is closed to narrow credentials until someone widens the table
		// in scope.go deliberately. See the comment there.
		if !p.Allows(r.Method, r.URL.Path) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this credential is scoped to " + string(p.Scope) +
					" and may not " + r.Method + " " + r.URL.Path,
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

// resolve identifies the caller, or returns an error if it cannot.
//
// The operator's token is compared first and in constant time. Only a
// credential that is not the operator's is looked up in storage, so the common
// path costs no query, and a timing difference between "wrong operator token"
// and "unknown token" reveals only which of two rejections happened.
func (a *Auth) resolve(r *http.Request) (Principal, error) {
	token, ok := presentedToken(r)
	if !ok {
		return Principal{}, errNoCredential
	}
	if a.constantTimeMatch(token) {
		return Principal{Scope: ScopeOperator}, nil
	}
	if a.Tokens == nil {
		return Principal{}, errNoCredential
	}
	row, found, err := a.Tokens.TokenByHash(HashToken(token))
	if err != nil || !found || row.Revoked != 0 {
		return Principal{}, errNoCredential
	}
	scope := Scope(row.Scope)
	if !scope.Valid() {
		// A row this binary cannot interpret is refused rather than guessed at.
		// Guessing on the permissive side is how a scope introduced by a newer
		// binary would silently become "operator" on an older one.
		return Principal{}, errNoCredential
	}
	return Principal{
		Scope: scope, TokenID: row.ID,
		Capability: row.Capability, Label: row.Label,
	}, nil
}

// presentedToken pulls the credential out of a request, from either place a
// client can put one.
//
// The query form is accepted only on the WebSocket upgrade paths, because the
// browser WebSocket API cannot set headers — refusing it would mean no browser
// client could ever connect. Restricting it to those paths keeps a token out of
// the HTTP access logs of ordinary API traffic.
func presentedToken(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return h[len(prefix):], true
		}
		return "", false
	}
	if isWebSocketPath(r.URL.Path) {
		if t := r.URL.Query().Get("token"); t != "" {
			return t, true
		}
	}
	return "", false
}

// constantTimeMatch compares in constant time. A byte-by-byte comparison here
// leaks the token one character at a time to anyone who can measure it.
func (a *Auth) constantTimeMatch(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) == 1
}

func isWebSocketPath(p string) bool {
	return p == "/ws/skill" || p == "/v1/stream"
}

// CheckOrigin decides whether a WebSocket upgrade may proceed.
//
// This is not the same question the token answers. A browser attaches
// credentials to cross-site WebSocket handshakes automatically, so a page on
// any other origin could drive a node the user is authenticated against —
// cross-site WebSocket hijacking. The Origin header is the only signal that
// distinguishes "the user's own control plane" from "a page that happens to be
// open in the same browser".
//
// A missing Origin is allowed: non-browser clients (the CLI, a skill, curl)
// do not send one, and they are exactly the callers for whom the token is the
// right check. A *present* Origin must be on the allowlist or match the host
// being served.
func (a *Auth) CheckOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser; the bearer token is the control here
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, allowed := range a.AllowedOrigins {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	// Same-origin: the UI is served by this very node, which is the common case
	// and must not need configuring.
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	// Behind a TLS-terminating proxy the Host differs from what the browser
	// saw, so same-origin cannot be established here. Opt-in only.
	return a.TrustedProxy
}

// ResolveListen turns the --listen flag into an address, defaulting to
// loopback.
//
// Binding every interface was the old default and it is the wrong one: a
// developer starting a node on a laptop in a café was serving it to the café.
// The safe address is the default and widening it is a visible act.
func ResolveListen(listen string, port int) (addr string, public bool, err error) {
	if listen == "" {
		return fmt.Sprintf("127.0.0.1:%d", port), false, nil
	}
	// A bare port or a bare host are both things people type.
	if !strings.Contains(listen, ":") {
		listen = net.JoinHostPort(listen, fmt.Sprint(port))
	}
	host, _, splitErr := net.SplitHostPort(listen)
	if splitErr != nil {
		return "", false, fmt.Errorf("invalid --listen %q: want host:port, :port or an address", listen)
	}
	return listen, !isLoopbackHost(host), nil
}

func isLoopbackHost(host string) bool {
	if host == "" {
		return false // an empty host in host:port form means every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
