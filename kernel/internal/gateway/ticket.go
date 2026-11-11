package gateway

// Watch tickets — a credential for one viewer of one session.
//
// # The gap
//
// `GET /v1/watch` lets many clients observe one live session, and it needed
// the operator's token, because a session's output *is* the conversation. That
// made the feature usable only from a backend: no application can hand the
// node's own authority to a browser in an audience, so every viewer had to be
// proxied through the application's servers, which is the hop the feature was
// supposed to remove.
//
// A ticket is the narrowest credential that removes it. It says exactly one
// thing — *the bearer may watch session S until time T* — and it is signed by
// the node, so the application's backend asks for one (with the operator
// token, once, server-side) and gives it to the viewer, who connects straight
// to the node.
//
// # Why it is stateless
//
// Scoped tokens (`aura token issue`) are rows: they are few, long-lived and
// revocable, which is right for an Expertise. Viewers are the opposite — many,
// short-lived, and never individually managed. A row per viewer would turn
// every page load in an audience into a write to the node's database. So a
// ticket carries its own claims and a MAC over them, and verifying one touches
// no storage at all.
//
// The price of stateless is the usual one: a ticket cannot be revoked before
// it expires. That is why the lifetime is bounded (a day at most, five minutes
// by default), why a ticket names a single session rather than a graph or a
// user, and why it dies with the session it names — a watcher on a session
// that has ended is disconnected, and a ticket for it attaches to nothing.
//
// # What it can never do
//
// A ticket authenticates `GET /v1/watch` and no other route, for the session
// it names and no other. It is not a token: it is refused in an Authorization
// header and on every other path, so there is no request it could be replayed
// into that reaches a graph, the ledger, the catalog or another session.
//
// The key is derived from the operator token rather than generated per
// process, so a ticket issued by a node is still honoured by the standby that
// replaces it — the viewer of a stream should not be thrown out by a failover
// the stream itself survives.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// ScopeWatch is the scope of a principal authenticated by a watch ticket.
	// Deliberately not accepted by Scope.Valid: that function vets rows in the
	// token table, and a watch principal is never a row.
	ScopeWatch Scope = "watch"

	defaultTicketTTL = 5 * time.Minute
	maxTicketTTL     = 24 * time.Hour
	ticketVersion    = 1
)

var errBadTicket = errors.New("invalid or expired watch ticket")

type ticketClaims struct {
	V       int    `json:"v"`
	Session string `json:"s"`
	Expires int64  `json:"e"` // unix seconds
}

// ticketKeys derives and caches the MAC key.
type ticketKeys struct {
	once sync.Once
	key  []byte
}

func (k *ticketKeys) get(operatorToken string) []byte {
	k.once.Do(func() {
		if operatorToken == "" {
			// A --no-auth node has no secret to derive from. It also has no
			// need for tickets — every caller is already the operator — so a
			// per-process key is enough to keep the format honest.
			k.key = make([]byte, 32)
			_, _ = rand.Read(k.key)
			return
		}
		mac := hmac.New(sha256.New, []byte(operatorToken))
		mac.Write([]byte("aura/watch-ticket/v1"))
		k.key = mac.Sum(nil)
	})
	return k.key
}

func signTicket(key []byte, c ticketClaims) string {
	body, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyTicket checks a ticket's MAC and expiry and returns its claims.
func verifyTicket(key []byte, ticket string, now time.Time) (ticketClaims, error) {
	var c ticketClaims
	payload, sig, ok := strings.Cut(ticket, ".")
	if !ok || payload == "" || sig == "" || len(ticket) > 1024 {
		return c, errBadTicket
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return c, errBadTicket
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	// Constant time, and before the payload is parsed: nothing an attacker
	// controls is interpreted until the node has confirmed it wrote it.
	if !hmac.Equal(got, mac.Sum(nil)) {
		return c, errBadTicket
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(body, &c) != nil {
		return c, errBadTicket
	}
	if c.V != ticketVersion || c.Session == "" || now.Unix() >= c.Expires {
		return c, errBadTicket
	}
	return c, nil
}

// watchTicketPrincipal authenticates a request by watch ticket, if it carries
// one on the only route a ticket is good for.
func (a *Auth) watchTicketPrincipal(r *http.Request) (Principal, bool) {
	if r.URL.Path != "/v1/watch" || r.Method != http.MethodGet {
		return Principal{}, false
	}
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		return Principal{}, false
	}
	claims, err := verifyTicket(a.tickets.get(a.Token), ticket, time.Now())
	if err != nil {
		return Principal{}, false
	}
	return Principal{Scope: ScopeWatch, Session: claims.Session}, true
}

// POST /v1/watch/tickets — mint a ticket for one viewer of one live session.
//
//	{"session": "<id>", "ttl_s": 300}  →  {"ticket": "...", "session": "...", "expires": 1794430982}
//
// Operator-only: issuing a ticket is deciding who may watch, which is the
// application's decision to make on its own server.
func (g *Gateway) issueWatchTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session string `json:"session"`
		TTLs    int64  `json:"ttl_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Session) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "`session` is required"})
		return
	}
	// Only for a session that exists now. A ticket for a session id that is
	// not live would be a credential waiting for whoever later opens a
	// session under that id.
	if _, ok := g.Mgr.Get(req.Session); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no live session " + req.Session})
		return
	}
	ttl := defaultTicketTTL
	if req.TTLs > 0 {
		ttl = time.Duration(req.TTLs) * time.Second
	}
	if ttl > maxTicketTTL {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "ttl_s is capped at 86400: a ticket cannot be revoked, so it must not outlive a day"})
		return
	}
	expires := time.Now().Add(ttl).Unix()
	token := ""
	if g.Auth != nil {
		token = g.Auth.Token
	}
	ticket := signTicket(g.ticketKeys().get(token), ticketClaims{
		V: ticketVersion, Session: req.Session, Expires: expires})
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket": ticket, "session": req.Session, "expires": expires})
}

// ticketKeys returns the key holder shared with the auth middleware, so what
// this node signs is what it verifies.
func (g *Gateway) ticketKeys() *ticketKeys {
	if g.Auth != nil {
		return &g.Auth.tickets
	}
	return &g.noAuthTickets
}
