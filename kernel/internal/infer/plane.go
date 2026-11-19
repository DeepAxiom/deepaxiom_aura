// Package infer is the node's inference plane: the place a request to a model
// passes through on its way to the engine.
//
// # The hole this closes
//
// An Expertise that calls a model has always called the engine directly. The
// node routed the question in and the answer out and never saw the inference
// in between — which engine served it, what it cost, whether it was still
// running after the chain it was for had been cancelled. Everything the node
// says about an inference it knows because an Expertise told it afterwards.
//
// Pointed here instead, the same request becomes something the node can
// answer for:
//
//   - **Whose work it is.** The request names the delivery it is being made
//     for, and the executor says what chain that is and what bounds it is
//     under. Nothing is signed and nothing is carried by the caller: the node
//     reads its own record. See Resolver.
//   - **When it should stop.** A cancelled chain, an ended session, a deadline
//     that has passed, a node that is shutting down — each closes the request
//     at the engine. Until now cancellation was a message to the Expertise
//     and a hope that it noticed.
//   - **What it cost.** Usage is read off the engine's own response, per
//     graph, whether or not the Expertise goes on to report any.
//
// # What this is not, yet
//
// A pass-through. There is no queue here: a request that arrives is sent.
// Ordering by priority and deadline, a lane kept free for urgent work, and
// evidence sealed into the ledger are the next phase, and they are built on
// what this one establishes — that the node is in the path, and knows what
// each request is for.
//
// # What it knows about engines
//
// As little as it can, and all of it here rather than in the executor: the
// shape of an OpenAI-compatible request and its streamed response, and how one
// engine or another spells a priority. No tokenizer, no chat template, no
// model.
package infer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Headers a caller uses to say which delivery a request is for.
const (
	HeaderSession  = "X-Aura-Session"
	HeaderDelivery = "X-Aura-Delivery"
	HeaderNode     = "X-Aura-Node"
	HeaderPort     = "X-Aura-Port"
	// HeaderRequest is set on every response: the id this node knows the
	// request by, for matching a caller's log line to the node's.
	HeaderRequest = "X-Aura-Inference"
)

// Ref is how a caller names the delivery it is working on.
type Ref struct {
	Session, Delivery, Node, Port string
}

// Who is the caller, as the node's authentication established it.
type Who struct {
	// Operator is the node's own authority. Such a caller cannot be checked
	// against a delivery and is believed.
	Operator bool
	// TokenID and Capability are a scoped credential's.
	TokenID    string
	Capability string
}

// Resolved is what the executor knows about a delivery.
type Resolved struct {
	Session, Graph, Root, Cause string
	Priority                    int
	Deadline                    int64 // unix millis; 0 = none
	Known, Verified, Cancelled  bool
}

// Resolver answers "whose work is this" from the executor's own record.
type Resolver func(Ref, Who) (Resolved, error)

var (
	// ErrForeign: the delivery was made to someone else. The request is refused.
	ErrForeign = errors.New("that delivery was made to another Expertise")
	// ErrUnplaced: the session or destination named is not one the node has.
	ErrUnplaced = errors.New("the delivery named is not one this node can place")
)

// Priority conventions an engine may use for its own `priority` field.
const (
	PriorityNone   = ""
	PriorityVLLM   = "vllm"   // lower value served first
	PrioritySGLang = "sglang" // higher value served first
)

// Config is how a node is told about its engine.
type Config struct {
	// Upstream is the engine's OpenAI-compatible base URL, e.g.
	// http://127.0.0.1:8000/v1.
	Upstream string
	// APIKey is sent to the engine as a bearer token. It is the engine's
	// credential, never the node's: a caller's own Authorization header stops
	// here and is not forwarded.
	APIKey string
	// Priority says how the engine spells a request's scheduling priority,
	// or PriorityNone when it has no such field.
	//
	// Only set this for an engine actually started to honour it. vLLM answers
	// a non-zero priority with an error unless it was launched with
	// --scheduling-policy priority, so naming the convention against a
	// default vLLM breaks every prioritised request.
	Priority string
	// MaxBodyBytes caps a request body. Zero means DefaultMaxBody.
	MaxBodyBytes int64
	// Version identifies the node to the engine.
	Version string
	Log     *slog.Logger

	// Transport and Now are for tests.
	Transport http.RoundTripper
	Now       func() time.Time
}

// DefaultMaxBody is generous on purpose: a vision request carries its images
// in the body, base64, and one camera frame is a megabyte.
const DefaultMaxBody = 64 << 20

// Plane is a node's inference plane.
type Plane struct {
	upstream     *url.URL
	apiKey       string
	prioritySign int
	priorityName string
	maxBody      int64
	version      string
	log          *slog.Logger
	now          func() time.Time
	client       *http.Client
	resolve      Resolver

	// usageSupported is whether the engine accepts `stream_options`. Assumed
	// until an engine says otherwise with a 400, then remembered: asking again
	// on every request would cost a failed round trip per message forever.
	usageSupported atomic.Bool
	closed         atomic.Bool
	seq            atomic.Uint64

	mu       sync.Mutex
	inflight map[uint64]*request

	stats  counters
	engine engineCache
}

// request is one inference in flight.
type request struct {
	id                   uint64
	session, root, cause string
	cancel               context.CancelCauseFunc
}

// stop is why a request was ended by the node rather than by the engine.
type stop struct{ kind, detail string }

func (s *stop) Error() string { return s.detail }

// Kinds of stop, as they appear in the node's counters.
const (
	StopChain       = "chain_cancelled"
	StopSpeculation = "speculation_superseded"
	StopSession     = "session_ended"
	StopDeadline    = "deadline"
	StopShutdown    = "node_stopping"
	StopCaller      = "caller_gone"
)

// New builds a plane for an engine. resolve may be nil, in which case no
// request is ever attributed to a delivery.
func New(cfg Config, resolve Resolver) (*Plane, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.Upstream), "/")
	if base == "" {
		return nil, errors.New("inference: no upstream URL")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("inference: %q is not an http(s) URL", cfg.Upstream)
	}
	sign, err := prioritySign(cfg.Priority)
	if err != nil {
		return nil, err
	}
	p := &Plane{
		upstream: u, apiKey: cfg.APIKey,
		prioritySign: sign, priorityName: strings.ToLower(strings.TrimSpace(cfg.Priority)),
		maxBody: cfg.MaxBodyBytes, version: cfg.Version,
		log: cfg.Log, now: cfg.Now, resolve: resolve,
		inflight: map[uint64]*request{},
	}
	if p.maxBody <= 0 {
		p.maxBody = DefaultMaxBody
	}
	if p.log == nil {
		p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if p.now == nil {
		p.now = time.Now
	}
	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{
			// The body is relayed, not read: a response this client quietly
			// decompressed would no longer be the bytes the engine sent.
			DisableCompression:  true,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
		}
	}
	// No client timeout. A generation takes as long as it takes; what ends one
	// early is the caller leaving, a cancel, or a deadline — each of which
	// arrives through the request's context.
	p.client = &http.Client{Transport: transport}
	p.usageSupported.Store(true)
	return p, nil
}

func prioritySign(name string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case PriorityNone:
		return 0, nil
	case PriorityVLLM:
		return -1, nil
	case PrioritySGLang:
		return 1, nil
	}
	return 0, fmt.Errorf("inference: unknown priority convention %q (want %q or %q)",
		name, PriorityVLLM, PrioritySGLang)
}

// Upstream is the engine this plane forwards to, without credentials.
func (p *Plane) Upstream() string { return p.upstream.Redacted() }

// routes maps what a caller asks this node for to what it asks the engine
// for. generative marks the ones that produce tokens and may stream.
var routes = map[string]struct {
	method     string
	generative bool
}{
	"chat/completions": {http.MethodPost, true},
	"completions":      {http.MethodPost, true},
	"embeddings":       {http.MethodPost, false},
	"rerank":           {http.MethodPost, false},
	"models":           {http.MethodGet, false},
}

// Routes lists the paths, relative to the plane's mount point, it serves.
func Routes() []string {
	out := make([]string, 0, len(routes))
	for path, r := range routes {
		out = append(out, r.method+" "+path)
	}
	sort.Strings(out)
	return out
}

// Serve handles one request. route is the path below the mount point
// ("chat/completions"); who is the authenticated caller.
func (p *Plane) Serve(w http.ResponseWriter, r *http.Request, who Who, route string) {
	spec, ok := routes[route]
	if !ok || spec.method != r.Method {
		fail(w, http.StatusNotFound, "not_found", "this node's inference plane serves: "+
			strings.Join(Routes(), ", "))
		return
	}
	if p.closed.Load() {
		fail(w, http.StatusServiceUnavailable, "node_stopping", "this node is stopping")
		return
	}
	id := p.seq.Add(1)
	w.Header().Set(HeaderRequest, strconv.FormatUint(id, 10))

	if route == "models" {
		p.passThrough(w, r, route)
		return
	}

	arrived := p.now()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("request body exceeds this node's limit of %d bytes", p.maxBody))
			return
		}
		fail(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return
	}
	b, err := parseBody(raw)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Whose work is this. A request that names no delivery is served and
	// counted as unattributed; one that names a delivery the node cannot
	// stand behind is refused, because a wrong attribution is worse than
	// none — it is somebody else's priority, or somebody else's bill.
	var res Resolved
	attributed := false
	ref := Ref{
		Session: r.Header.Get(HeaderSession), Delivery: r.Header.Get(HeaderDelivery),
		Node: r.Header.Get(HeaderNode), Port: r.Header.Get(HeaderPort),
	}
	if ref.Session != "" && p.resolve != nil {
		res, err = p.resolve(ref, who)
		switch {
		case errors.Is(err, ErrForeign):
			p.stats.refused.Add(1)
			fail(w, http.StatusForbidden, "not_your_delivery",
				"the delivery this request names was made to another Expertise")
			return
		case err != nil:
			p.stats.refused.Add(1)
			fail(w, http.StatusConflict, "unknown_delivery",
				"this node cannot place the delivery this request names: "+err.Error())
			return
		}
		attributed = true
		if res.Cancelled {
			p.stats.refused.Add(1)
			fail(w, http.StatusConflict, "cancelled",
				"the chain this request belongs to has been cancelled")
			return
		}
		if res.Deadline != 0 && p.now().UnixMilli() >= res.Deadline {
			// Already late. Sending it would cost the engine a prompt
			// evaluation for an answer the kernel is going to drop.
			p.stats.refused.Add(1)
			fail(w, http.StatusRequestTimeout, "deadline_exceeded",
				"the deadline of the delivery this request belongs to has passed")
			return
		}
	}
	g := p.stats.graph(res.Graph)
	g.requests.Add(1)

	// --- what the node changes in the request --------------------------
	//
	// Usage: asked for on the node's own account, so the cost of a request is
	// read off the engine and not taken on an Expertise's word. Remembered as
	// forced so the extra chunk can be taken back out of the stream.
	stream := b.isTrue("stream")
	forcedUsage := false
	original, hadOptions := b.get("stream_options")
	if spec.generative && stream && !asksForUsage(original) && p.usageSupported.Load() {
		b.set("stream_options", withUsage(original))
		forcedUsage = true
	}
	// Priority: the node's to state. A caller's own value is taken out
	// whatever the engine is — an Expertise does not get to name its own
	// urgency — and the delivery's is written in the engine's convention when
	// the node was told the engine has one.
	b.remove("priority")
	if p.prioritySign != 0 && attributed && res.Priority != 0 {
		b.set("priority", []byte(strconv.Itoa(p.prioritySign*res.Priority)))
	}

	// --- one context that every reason to stop arrives through ----------
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	if res.Deadline != 0 {
		var release context.CancelFunc
		ctx, release = context.WithDeadlineCause(ctx, time.UnixMilli(res.Deadline),
			&stop{StopDeadline, "the delivery's deadline passed"})
		defer release()
	}
	req := &request{id: id, session: res.Session, root: res.Root, cause: res.Cause, cancel: cancel}
	p.track(req)
	defer p.untrack(id)
	// Closed between the check above and now: the request must not outlive a
	// shutdown that has already swept the table.
	if p.closed.Load() {
		cancel(&stop{StopShutdown, "this node is stopping"})
	}

	sent := p.now()
	p.stats.prepareMicros.Add(uint64(sent.Sub(arrived).Microseconds()))
	p.stats.prepared.Add(1)
	g.inflight.Add(1)
	defer g.inflight.Add(-1)

	resp, err := p.send(ctx, r, route, b.bytes())
	if err == nil && forcedUsage && resp.StatusCode == http.StatusBadRequest {
		// Possibly an engine that does not know `stream_options`. Ask again
		// exactly as the caller did; if that is accepted, the option was the
		// reason and is not sent again.
		retry := *b
		retry.members = append([]member(nil), b.members...)
		if hadOptions {
			retry.set("stream_options", original)
		} else {
			retry.remove("stream_options")
		}
		second, rerr := p.send(ctx, r, route, retry.bytes())
		if rerr == nil && second.StatusCode != http.StatusBadRequest {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			resp, forcedUsage = second, false
			p.usageSupported.Store(false)
			p.log.Info("the engine refused stream_options; usage will be read only where it is offered",
				"upstream", p.Upstream())
		} else if rerr == nil {
			second.Body.Close() // both refused: the first answer is the answer
		}
	}
	if err != nil {
		kind := p.stopKind(ctx, r)
		g.finish(kind, false)
		p.stats.stopped(kind)
		switch kind {
		case StopCaller:
			// Nobody to answer.
		case StopDeadline:
			fail(w, http.StatusRequestTimeout, "deadline_exceeded", context.Cause(ctx).Error())
		case "":
			p.log.Warn("the engine could not be reached", "upstream", p.Upstream(), "err", err)
			fail(w, http.StatusBadGateway, "engine_unreachable",
				"this node could not reach its inference engine")
		default:
			fail(w, http.StatusConflict, "cancelled", context.Cause(ctx).Error())
		}
		return
	}
	defer resp.Body.Close()

	// --- relay ---------------------------------------------------------
	for _, h := range []string{"Content-Type", "Cache-Control", "X-Request-Id"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	flush := func() { _ = rc.Flush() }

	var obs observation
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		err = relaySSE(w, flush, resp.Body, &obs, forcedUsage, p.now)
	} else {
		err = relayJSON(w, resp.Body, &obs, p.now)
		flush()
	}

	kind := ""
	if err != nil {
		kind = p.stopKind(ctx, r)
		if kind == "" {
			p.log.Debug("an inference response ended early", "err", err)
		}
	}
	ok2xx := resp.StatusCode >= 200 && resp.StatusCode < 300
	g.finish(kind, err == nil && ok2xx)
	p.stats.stopped(kind)
	if obs.usage.Reported {
		g.metered.Add(1)
		g.input.Add(uint64(obs.usage.Input))
		g.output.Add(uint64(obs.usage.Output))
		g.cached.Add(uint64(obs.usage.Cached))
	}
	if !obs.firstToken.IsZero() {
		g.firstToken.observe(obs.firstToken.Sub(sent))
	}
	p.log.Debug("inference relayed",
		"id", id, "route", route, "graph", res.Graph, "session", res.Session,
		"attributed", attributed, "verified", res.Verified, "status", resp.StatusCode,
		"stopped", kind, "input", obs.usage.Input, "output", obs.usage.Output,
		"cached", obs.usage.Cached, "usage_reported", obs.usage.Reported,
		"first_token_ms", msSince(sent, obs.firstToken), "total_ms", p.now().Sub(sent).Milliseconds())
}

func msSince(from, to time.Time) int64 {
	if to.IsZero() {
		return -1
	}
	return to.Sub(from).Milliseconds()
}

// stopKind names why a request ended early, or "" when the node did not end
// it and the caller did not leave — that is, when the engine failed.
func (p *Plane) stopKind(ctx context.Context, r *http.Request) string {
	var s *stop
	if errors.As(context.Cause(ctx), &s) {
		return s.kind
	}
	if r.Context().Err() != nil {
		return StopCaller
	}
	return ""
}

// send forwards a request to the engine.
func (p *Plane) send(ctx context.Context, r *http.Request, route string, payload []byte) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, p.upstream.String()+"/"+route, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		out.Header.Set("Content-Type", "application/json")
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		out.Header.Set("Accept", accept)
	}
	// The caller's Authorization is its credential for this node and stops
	// here. The engine gets the engine's own, or none.
	if p.apiKey != "" {
		out.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	out.Header.Set("User-Agent", "aura/"+p.version)
	return p.client.Do(out)
}

// passThrough relays a request the node has nothing to say about.
func (p *Plane) passThrough(w http.ResponseWriter, r *http.Request, route string) {
	resp, err := p.send(r.Context(), r, route, nil)
	if err != nil {
		if r.Context().Err() == nil {
			fail(w, http.StatusBadGateway, "engine_unreachable",
				"this node could not reach its inference engine")
		}
		return
	}
	defer resp.Body.Close()
	if v := resp.Header.Get("Content-Type"); v != "" {
		w.Header().Set("Content-Type", v)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Plane) track(req *request) {
	p.mu.Lock()
	p.inflight[req.id] = req
	p.mu.Unlock()
}

func (p *Plane) untrack(id uint64) {
	p.mu.Lock()
	delete(p.inflight, id)
	p.mu.Unlock()
}

// Cancel ends the requests in flight for abandoned work. root and cause are
// the two ways the executor names it — a whole chain by its root, one
// delivery by its cause; both empty ends everything the session has running.
//
// It only closes: the cleanup happens on each request's own goroutine. So it
// is safe to call from the routing path, which is where it is called from.
func (p *Plane) Cancel(session, root, cause, kind, detail string) int {
	if session == "" {
		return 0
	}
	p.mu.Lock()
	var hit []*request
	for _, req := range p.inflight {
		if req.session != session {
			continue
		}
		whole := root == "" && cause == ""
		if whole || (root != "" && req.root == root) || (cause != "" && req.cause == cause) {
			hit = append(hit, req)
		}
	}
	p.mu.Unlock()
	for _, req := range hit {
		req.cancel(&stop{kind, detail})
	}
	return len(hit)
}

// Shutdown ends every request in flight and refuses new ones.
//
// Called before the node's HTTP server starts waiting for its handlers to
// return. Without it a node stopped during a generation would wait out its
// whole drain window on a handler that is itself waiting on a model — and that
// window is the same ten seconds a container runtime allows before it kills
// the process, so the store would never be closed.
func (p *Plane) Shutdown() int {
	p.closed.Store(true)
	p.mu.Lock()
	hit := make([]*request, 0, len(p.inflight))
	for _, req := range p.inflight {
		hit = append(hit, req)
	}
	p.mu.Unlock()
	for _, req := range hit {
		req.cancel(&stop{StopShutdown, "this node is stopping"})
	}
	return len(hit)
}

// InFlight is how many requests are at the engine right now.
func (p *Plane) InFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inflight)
}

// fail answers with an error in the shape an OpenAI client already knows how
// to show, so the reason reaches whoever is reading the Expertise's log.
func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": "aura_inference", "code": code},
	})
}

// Guard runs fn and keeps a panic in it from taking the node down.
//
// The HTTP server recovers a panic on a handler's own goroutine. It does not
// recover one on a goroutine the handler started, and a panic there ends the
// process — every session, every connection, the ledger's writer. Anything
// this package starts goes through here.
func (p *Plane) Guard(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("panic in the inference plane; the request failed, the node did not",
				"in", what, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}
