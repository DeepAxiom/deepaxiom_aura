package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/executor"
	"aura/kernel/internal/identity"
)

// Inbound HTTP ingress — the one piece of connectivity that cannot live
// outside the kernel.
//
// Skills connect out to the kernel, which is what lets them run behind NAT and
// a corporate firewall with no inbound ports. That inversion is load-bearing
// everywhere except here: Stripe, GitHub and every other webhook source dials
// *in*, and cannot reach a skill that has no address. Something with a public
// port has to accept the request and turn it into an envelope. That is
// transport, which is the kernel's job — so this is a general ingress
// primitive, not a webhook feature: any push-based source lands the same way.
//
//	POST   /v1/ingress          declare a route
//	GET    /v1/ingress          list routes (secrets are never returned)
//	DELETE /v1/ingress/{name}   revoke a route
//	POST   /hooks/{name}        the endpoint you give the external system
//
// Each delivery opens its own session on the target graph, so a webhook gets
// the same causal event log as any other work: `aura why <session>` explains
// what a delivery did.

const (
	maxIngressBody    = 4 << 20         // same cap the projection host uses
	ingressSessionTTL = 2 * time.Minute // how long a delivery's session lives
	// replayWindow is how long a delivery digest is remembered. It bounds both
	// the replay exposure and the size of the seen-table; five minutes is the
	// tolerance Stripe and most webhook senders use for their own timestamps.
	defaultReplayWindow = 5 * time.Minute
	// defaultRouteRate caps deliveries per minute on one route. A public
	// endpoint that opens a session per request needs a ceiling that does not
	// depend on the sender behaving.
	defaultRouteRate = 600
)

var ingressNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// IngressRoute is one declared inbound endpoint.
type IngressRoute struct {
	Name  string `json:"name"`
	Graph string `json:"graph"`
	// Port and Schema decide how the body enters the graph. Defaults match the
	// seeded graphs so a route needs neither in the common case.
	Port   string `json:"port,omitempty"`
	Schema string `json:"schema,omitempty"`

	// SecretEnv names the environment variable holding the shared secret. The
	// secret itself is never stored and never returned — only where to find it.
	SecretEnv         string `json:"secret_env,omitempty"`
	SignatureHeader   string `json:"signature_header,omitempty"`
	SignaturePrefix   string `json:"signature_prefix,omitempty"`   // e.g. "sha256="
	SignatureEncoding string `json:"signature_encoding,omitempty"` // hex | base64

	// TimestampHeader names a header carrying the send time, when the sender
	// provides one (Stripe does; GitHub does not). Given it, the signed payload
	// becomes "<timestamp>.<body>" and deliveries outside ToleranceSeconds are
	// refused — which stops a replay before it is ever stored.
	//
	// Without it the digest table below is the only replay defence, which is
	// weaker only in that it must remember rather than compute.
	TimestampHeader  string `json:"timestamp_header,omitempty"`
	ToleranceSeconds int    `json:"tolerance_seconds,omitempty"`

	// RatePerMinute caps deliveries on this route. 0 takes the default.
	RatePerMinute int `json:"rate_per_minute,omitempty"`

	// Unsigned is the explicit opt-out. Verification is on by default: an
	// unauthenticated public endpoint that injects into a graph is something a
	// user should have to ask for in writing, not something they get by
	// forgetting a field.
	Unsigned bool `json:"unsigned,omitempty"`
}

func (r *IngressRoute) tolerance() time.Duration {
	if r.ToleranceSeconds > 0 {
		return time.Duration(r.ToleranceSeconds) * time.Second
	}
	return defaultReplayWindow
}

func (r *IngressRoute) rateLimit() int {
	if r.RatePerMinute > 0 {
		return r.RatePerMinute
	}
	return defaultRouteRate
}

func (r *IngressRoute) normalize() error {
	r.Name = strings.ToLower(strings.TrimSpace(r.Name))
	if !ingressNameRe.MatchString(r.Name) {
		return fmt.Errorf("invalid name %q (want lowercase letters, digits and dashes)", r.Name)
	}
	if strings.TrimSpace(r.Graph) == "" {
		return fmt.Errorf("route %q: graph is required", r.Name)
	}
	if r.Port == "" {
		r.Port = "text_out"
	}
	if r.Schema == "" {
		r.Schema = "std/text@1"
	}
	if r.SignatureHeader == "" {
		r.SignatureHeader = "X-Signature-256"
	}
	if r.SignatureEncoding == "" {
		r.SignatureEncoding = "hex"
	}
	if r.SignatureEncoding != "hex" && r.SignatureEncoding != "base64" {
		return fmt.Errorf("route %q: signature_encoding must be hex or base64", r.Name)
	}
	if r.Unsigned {
		return nil
	}
	if r.SecretEnv == "" {
		return fmt.Errorf(
			"route %q: secret_env is required — set it to the name of an environment "+
				"variable holding the shared secret, or set \"unsigned\": true to accept "+
				"unauthenticated deliveries deliberately", r.Name)
	}
	if os.Getenv(r.SecretEnv) == "" {
		return fmt.Errorf(
			"route %q: environment variable %s is empty, so every delivery would be "+
				"rejected", r.Name, r.SecretEnv)
	}
	return nil
}

// verify checks the request signature against the shared secret in constant
// time and, where the sender provides a timestamp, that the delivery is
// recent. It returns the signature actually presented, which becomes the
// delivery's replay digest and its idempotency key.
//
// A byte-by-byte signature comparison would leak the expected digest one
// character at a time, hence hmac.Equal.
func (r *IngressRoute) verify(sigHeader, tsHeader string, body []byte) (signature string, err error) {
	if r.Unsigned {
		// Nothing was signed, so the body itself is all there is to identify
		// the delivery by. Still enough to catch a verbatim replay.
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:]), nil
	}
	secret := os.Getenv(r.SecretEnv)
	if secret == "" {
		return "", fmt.Errorf("server secret unavailable")
	}
	got := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sigHeader), r.SignaturePrefix))
	if got == "" {
		return "", fmt.Errorf("missing signature")
	}

	// What was signed. Including the timestamp is what makes the signature
	// bind the delivery to a moment rather than to a body that stays valid
	// forever.
	signed := body
	if r.TimestampHeader != "" {
		ts := strings.TrimSpace(tsHeader)
		if ts == "" {
			return "", fmt.Errorf("missing %s", r.TimestampHeader)
		}
		if err := r.checkFreshness(ts); err != nil {
			return "", err
		}
		signed = append([]byte(ts+"."), body...)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(signed)
	sum := mac.Sum(nil)

	var want string
	if r.SignatureEncoding == "base64" {
		want = base64.StdEncoding.EncodeToString(sum)
	} else {
		want = hex.EncodeToString(sum)
	}
	if !hmac.Equal([]byte(strings.ToLower(got)), []byte(strings.ToLower(want))) {
		return "", fmt.Errorf("signature mismatch")
	}
	return got, nil
}

// checkFreshness rejects a delivery whose timestamp is outside the tolerance.
// Both directions matter: an old timestamp is a replay, and one far in the
// future is a sender whose clock cannot be trusted to bound anything.
func (r *IngressRoute) checkFreshness(ts string) error {
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%s is not a unix timestamp", r.TimestampHeader)
	}
	drift := time.Since(time.Unix(seconds, 0))
	if drift < 0 {
		drift = -drift
	}
	if drift > r.tolerance() {
		return fmt.Errorf("delivery is %s outside the %s tolerance",
			drift.Round(time.Second), r.tolerance())
	}
	return nil
}

// routeCache holds the declared ingress routes in memory.
//
// Every delivery used to read *every* route out of SQLite to find one, which
// put a table scan on the hot path of the endpoint most exposed to traffic the
// node does not control. Routes change only when someone declares or revokes
// one, so the cache is invalidated there rather than expiring on a timer.
type routeCache struct {
	mu     sync.RWMutex
	routes map[string]IngressRoute
	loaded bool
}

func (g *Gateway) routeStore() *routeCache {
	g.routesOnce.Do(func() { g.routes = &routeCache{routes: map[string]IngressRoute{}} })
	return g.routes
}

// invalidateRoutes drops the cache after a declare or a revoke. A revoked
// inbound URL that keeps working is a security problem, so this is called on
// the write path rather than left to a TTL.
func (g *Gateway) invalidateRoutes() {
	c := g.routeStore()
	c.mu.Lock()
	c.loaded = false
	c.routes = map[string]IngressRoute{}
	c.mu.Unlock()
}

func (g *Gateway) loadRoute(name string) (IngressRoute, bool) {
	c := g.routeStore()

	c.mu.RLock()
	if c.loaded {
		route, ok := c.routes[name]
		c.mu.RUnlock()
		return route, ok
	}
	c.mu.RUnlock()

	all, err := g.St.LoadIngress()
	if err != nil {
		g.Log.Error("load ingress failed", "err", err)
		return IngressRoute{}, false
	}
	fresh := make(map[string]IngressRoute, len(all))
	for routeName, raw := range all {
		var route IngressRoute
		if err := json.Unmarshal(raw, &route); err != nil {
			g.Log.Error("stored ingress route is corrupt", "name", routeName, "err", err)
			continue
		}
		fresh[routeName] = route
	}

	c.mu.Lock()
	c.routes = fresh
	c.loaded = true
	c.mu.Unlock()

	route, ok := fresh[name]
	return route, ok
}

// declareIngress creates or replaces a route (POST /v1/ingress).
func (g *Gateway) declareIngress(w http.ResponseWriter, r *http.Request) {
	var route IngressRoute
	if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if err := route.normalize(); err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	// Fail now rather than on the first delivery from an external system,
	// where the error would surface as an unexplained 500 in their dashboard.
	if _, err := g.St.LoadGraph(route.Graph); err != nil {
		writeJSON(w, 422, map[string]string{
			"error": fmt.Sprintf("graph %q is not registered", route.Graph)})
		return
	}
	blob, _ := json.Marshal(route)
	if err := g.St.SaveIngress(route.Name, blob); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	g.invalidateRoutes()
	g.Log.Info("ingress route declared", "name", route.Name, "graph", route.Graph,
		"signed", !route.Unsigned)
	writeJSON(w, 201, map[string]any{"route": route, "url": "/hooks/" + route.Name})
}

func (g *Gateway) listIngress(w http.ResponseWriter, _ *http.Request) {
	all, err := g.St.LoadIngress()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := []IngressRoute{}
	for name, raw := range all {
		var route IngressRoute
		if err := json.Unmarshal(raw, &route); err != nil {
			g.Log.Error("stored ingress route is corrupt", "name", name, "err", err)
			continue
		}
		out = append(out, route)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, map[string]any{"ingress": out})
}

func (g *Gateway) deleteIngress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := g.loadRoute(name); !ok {
		writeJSON(w, 404, map[string]string{"error": "no ingress route " + name})
		return
	}
	if err := g.St.DeleteIngress(name); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	g.invalidateRoutes()
	g.Log.Info("ingress route revoked", "name", name)
	writeJSON(w, 200, map[string]string{"revoked": name})
}

// receiveHook is the public endpoint (POST /hooks/{name}).
//
// The order of the checks is the design. Each one is cheaper than the next and
// each is capable of rejecting the request on its own, so an attacker with a
// captured delivery never reaches the expensive part:
//
//	route exists → rate limit → body size → signature+freshness → replay → run
func (g *Gateway) receiveHook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	route, ok := g.loadRoute(name)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no ingress route " + name})
		return
	}

	if !g.hookRate(name, route.rateLimit()) {
		g.Log.Warn("ingress rate limit hit", "name", name, "limit", route.rateLimit())
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": fmt.Sprintf("route %q is limited to %d deliveries/min", name, route.rateLimit()),
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxIngressBody))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "cannot read body: " + err.Error()})
		return
	}

	signature, err := route.verify(
		r.Header.Get(route.SignatureHeader),
		r.Header.Get(route.TimestampHeader),
		body,
	)
	if err != nil {
		g.Log.Warn("ingress delivery rejected", "name", name, "reason", err)
		writeJSON(w, 401, map[string]string{"error": "signature verification failed"})
		return
	}

	// The digest identifies this exact delivery. A replay presents the same
	// signature over the same body, so it collides here and goes no further.
	digest := deliveryDigest(signature)
	seen, err := g.St.SeenIngressDelivery(name, digest, route.tolerance())
	if err != nil {
		g.Log.Error("ingress replay check failed", "name", name, "err", err)
		writeJSON(w, 500, map[string]string{"error": "replay check failed"})
		return
	}
	if seen {
		// 200, not an error: a webhook sender retrying a delivery it already
		// made is behaving correctly under at-least-once, and answering it with
		// a failure would make it retry harder. The work is simply not redone.
		g.Log.Info("ingress replay suppressed", "name", name)
		writeJSON(w, 200, map[string]string{
			"status": "already processed", "graph": route.Graph,
		})
		return
	}

	// One session per delivery: the webhook gets its own causal log, so
	// `aura why <session>` explains exactly what that event caused.
	sessionID := identity.NewSessionID()
	// The sender gets a 2xx, not the graph's output, so nothing is written
	// back to it — the causal log is where a delivery's result is read from.
	sess, err := g.Mgr.Start(sessionID, route.Graph, "", func([]byte, string) error { return nil })
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": err.Error()})
		return
	}

	env := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: sessionID,
		Node: executor.ClientRef, Port: route.Port, Seq: 1,
		// Derived from the delivery, not minted fresh. C3 rule 2 makes `idem`
		// the deduplication key for the whole downstream chain, so a random one
		// meant every retry looked like new work to every skill it reached.
		Idem:   fmt.Sprintf("hook:%s:%s", name, digest),
		Schema: route.Schema, Kind: channel.KindData,
		Payload: hookPayload(body, r.Header.Get("Content-Type")),
	}
	sess.Route(env)

	// The external system wants a fast 2xx, not the graph's result, so the
	// session outlives the request. It is handed to one reaper rather than
	// given a goroutine of its own: a goroutine per delivery that sleeps for
	// two minutes turns a burst into thousands of parked stacks, on the one
	// endpoint whose call rate the node does not control.
	g.reapSession(sessionID)

	writeJSON(w, 202, map[string]string{"session": sessionID, "graph": route.Graph})
}

// deliveryDigest reduces a signature to a fixed-size key. Hashed rather than
// stored raw so the seen-table never holds a value an attacker could replay if
// they read it.
func deliveryDigest(signature string) string {
	sum := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(sum[:])
}

// hookPayload passes JSON through untouched and wraps anything else as
// std/text@1, so a graph expecting text still gets something it can read.
func hookPayload(body []byte, contentType string) json.RawMessage {
	if strings.Contains(strings.ToLower(contentType), "json") && json.Valid(body) {
		return json.RawMessage(body)
	}
	wrapped, _ := json.Marshal(map[string]any{
		"text": string(body), "final": true,
	})
	return wrapped
}
