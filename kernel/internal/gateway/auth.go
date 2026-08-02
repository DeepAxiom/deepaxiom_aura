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
// Antes de este archivo, none of this existed: no token, no TLS, no origin
// check, y encima escuchaba en cada interfaz de red. Cualquiera que llegara
// al puerto podía registrar un grafo y correrlo — lo cual dejaba cada otra
// garantía del kernel condicionada a "que no haya nadie más en la red", que
// no es una garantía, es wishful thinking.
//
// Tres cosas cierran ese hueco, y van separadas a propósito (no es capricho,
// cada una contesta una pregunta distinta):
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

// Auth holds the node's access configuration.
type Auth struct {
	// Token is the bearer token required on the control surface. Empty
	// disables authentication entirely — only reachable via --no-auth, which
	// exists for a single-user loopback node and prints a warning.
	Token string
	// AllowedOrigins is the exact-match allowlist for the WebSocket Origin
	// header. Empty means "same-origin only": a browser page served from
	// somewhere else is refused.
	AllowedOrigins []string
	// TrustedProxy relaxes the origin check for deployments that terminate TLS
	// upstream. Off by default.
	TrustedProxy bool
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
//   - the UI's static assets — a browser must be able to load the page before
//     it can present a token. The assets contain no data; everything the page
//     then *asks for* goes through /v1/* and is authenticated.
//
// Note what is deliberately absent: /v1/skills and the A2A card. The
// capability catalog tells an attacker exactly which effects this node can
// produce, which is the most useful thing they could learn.
func openPath(p string) bool {
	switch {
	case p == "/healthz":
		return true
	case strings.HasPrefix(p, "/hooks/"):
		return true
	case isUIAsset(p):
		return true
	}
	return false
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
		if a.Token == "" || openPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !a.tokenOK(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="aura"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "missing or invalid bearer token — find it in <data-dir>/" + tokenFile,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) tokenOK(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return a.constantTimeMatch(h[len(prefix):])
		}
		return false
	}
	if isWebSocketPath(r.URL.Path) {
		return a.constantTimeMatch(r.URL.Query().Get("token"))
	}
	return false
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
