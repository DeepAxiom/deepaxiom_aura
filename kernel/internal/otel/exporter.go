package otel

// Exporter ships spans to an OTLP/HTTP collector while the node runs.
//
// `aura trace <session>` exports a session after the fact, which is the right
// tool for one conversation someone is already asking about. It is no use for
// the question an operator has under load — *which* of two thousand live
// sessions is slow — because that needs the spans in a backend that can be
// queried across sessions, as they happen.
//
// # The one rule: it must not be able to slow the node down
//
// Every envelope passes the point this hooks into, on the path that holds a
// session's delivery. So:
//
//   - Observe never blocks. It is one non-blocking channel send; if the queue
//     is full the span is dropped and counted.
//   - Nothing on the delivery path parses, hashes or encodes anything. The raw
//     envelope bytes are handed over and decoded on the exporter's goroutine.
//   - A collector that is down, slow or refusing costs a counter, never a
//     delivery. A failed batch is dropped, not retried into a growing backlog:
//     telemetry that arrives minutes late and out of order is noise, and a
//     retry queue is how an observability outage becomes a memory outage.
//
// # Sampling keeps whole sessions
//
// Sampling per span would export trees with holes in them. So the decision is
// made per session, from a hash of the session id: a session is exported
// entirely or not at all, and the same session gets the same answer on every
// node and after every restart. At 10% an operator sees one conversation in
// ten, complete.
//
// What sampling does not touch is the record. The causal log and the effect
// ledger hold every envelope and every effect regardless of what is exported;
// a session that was sampled out is still fully answerable with `aura why`
// and `aura trace`. Spans are a view for finding things, not the evidence.
//
// # Payloads are not exported
//
// A span carries the envelope's shape — who sent what kind of thing on which
// port, when, caused by what, under which model, costing how many tokens —
// and never its payload. Shipping user content to a telemetry backend is a
// retention decision, and it is not this exporter's to make by default.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aura/kernel/internal/channel"
)

// Config is how an Exporter is wired.
type Config struct {
	// Endpoint is the collector's base URL; spans go to <Endpoint>/v1/traces.
	Endpoint string
	// Headers are sent with every export — a collector's API key, typically.
	Headers map[string]string
	// SampleRatio is the fraction of sessions exported, 0..1. Values outside
	// that range are clamped; zero exports nothing.
	SampleRatio float64
	// Version is this node's version, reported as the scope version.
	Version string
	// NodeID, when set, is reported as a resource attribute so spans from
	// several nodes can be told apart.
	NodeID string
	Log    *slog.Logger

	// QueueSize bounds spans waiting to be exported. Zero means 8192.
	QueueSize int
	// BatchSize is the most spans sent in one request. Zero means 512.
	BatchSize int
	// FlushEvery is the longest a span waits for its batch to fill. Zero
	// means one second.
	FlushEvery time.Duration
	// Client is the HTTP client to use; nil means one with a 10s timeout.
	Client *http.Client
}

// Stats is what an exporter reports about itself.
type Stats struct {
	// Exported spans a collector acknowledged.
	Exported uint64 `json:"exported"`
	// Dropped spans that found the queue full.
	Dropped uint64 `json:"dropped"`
	// Failed spans in batches the collector did not accept.
	Failed uint64 `json:"failed"`
	// SampledOut envelopes belonging to sessions the ratio excluded.
	SampledOut uint64 `json:"sampled_out"`
}

type observed struct {
	session string
	raw     []byte
	ts      int64
}

// Exporter is safe for concurrent use.
type Exporter struct {
	cfg       Config
	url       string
	threshold uint64
	queue     chan observed
	stop      chan struct{}
	once      sync.Once
	wg        sync.WaitGroup

	exported, dropped, failed, sampledOut atomic.Uint64
	// failing is true while the collector is refusing batches, so the
	// transition is logged once rather than every batch.
	failing atomic.Bool

	// Known only to the run goroutine: which envelopes have been exported (so
	// a span is only given a parent the backend has), and each one's trace
	// context (so a skill's reply inherits its cause's). Bounded FIFO.
	seen *boundedMap
}

// NewExporter starts an exporter. Close it to flush and stop.
func NewExporter(cfg Config) (*Exporter, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("an OTLP exporter needs a collector endpoint")
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return nil, fmt.Errorf("OTLP endpoint %q must be an http:// or https:// URL", cfg.Endpoint)
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 8192
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 512
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	ratio := cfg.SampleRatio
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	x := &Exporter{
		cfg:   cfg,
		url:   endpoint + "/v1/traces",
		queue: make(chan observed, cfg.QueueSize),
		stop:  make(chan struct{}),
		seen:  newBoundedMap(1 << 16),
	}
	// Compared against the top 53 bits of the session hash, so the ratio is
	// exact in float64 and 1.0 admits every session.
	x.threshold = uint64(ratio * float64(uint64(1)<<53))
	x.wg.Add(1)
	go x.run()
	return x, nil
}

// Sampled reports whether a session's spans are exported.
func (x *Exporter) Sampled(session string) bool {
	return sessionHash(session)>>11 < x.threshold
}

// sessionHash is FNV-1a with a finishing mix. Not cryptographic and it does
// not need to be: the only requirements are that it is the same on every node
// and after every restart, that it spreads session ids evenly, and that it
// costs next to nothing, because it runs once per envelope on the delivery
// path. FNV alone leaves the high bits of short inputs poorly mixed, which is
// where the threshold comparison looks, hence the finisher.
func sessionHash(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// Observe offers one persisted envelope to the exporter. It never blocks.
func (x *Exporter) Observe(session string, envelope []byte, tsMillis int64) {
	if x == nil {
		return
	}
	if !x.Sampled(session) {
		x.sampledOut.Add(1)
		return
	}
	select {
	case x.queue <- observed{session: session, raw: envelope, ts: tsMillis}:
	default:
		x.dropped.Add(1)
	}
}

// Stats snapshots the counters.
func (x *Exporter) Stats() Stats {
	if x == nil {
		return Stats{}
	}
	return Stats{
		Exported:   x.exported.Load(),
		Dropped:    x.dropped.Load(),
		Failed:     x.failed.Load(),
		SampledOut: x.sampledOut.Load(),
	}
}

// Close flushes what is queued and stops. Bounded: a collector that has gone
// away must not hold a node's shutdown.
func (x *Exporter) Close() {
	if x == nil {
		return
	}
	x.once.Do(func() { close(x.stop) })
	done := make(chan struct{})
	go func() { x.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (x *Exporter) run() {
	defer x.wg.Done()
	ticker := time.NewTicker(x.cfg.FlushEvery)
	defer ticker.Stop()
	batch := make([]map[string]any, 0, x.cfg.BatchSize)
	flush := func() {
		if len(batch) > 0 {
			x.send(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case <-x.stop:
			// Whatever is already queued goes out; nothing new is waited for.
			for {
				select {
				case o := <-x.queue:
					if span := x.span(o); span != nil {
						batch = append(batch, span)
						if len(batch) >= x.cfg.BatchSize {
							flush()
						}
					}
				default:
					flush()
					return
				}
			}
		case <-ticker.C:
			flush()
		case o := <-x.queue:
			if span := x.span(o); span != nil {
				batch = append(batch, span)
				if len(batch) >= x.cfg.BatchSize {
					flush()
				}
			}
		}
	}
}

func (x *Exporter) span(o observed) map[string]any {
	var e channel.Envelope
	if json.Unmarshal(o.raw, &e) != nil || e.ID == "" {
		return nil
	}
	trace := ""
	if channel.ValidTraceparent(e.Trace) {
		trace = e.Trace
	} else if e.CauseID != "" {
		trace, _ = x.seen.get(e.CauseID)
	}
	_, parentKnown := x.seen.get(e.CauseID)
	x.seen.put(e.ID, trace)

	in := SpanInput{
		Session: o.session, Envelope: e, TSMillis: o.ts,
		ParentKnown: e.CauseID != "" && parentKnown,
		Trace:       trace, WithSession: true,
	}
	if e.Kind == channel.KindError {
		// The state, never the detail: an error's detail is skill-authored
		// text and can quote the input that caused it.
		in.ErrorMessage = "error"
	}
	return Span(in)
}

func (x *Exporter) send(spans []map[string]any) {
	var resource []map[string]any
	if x.cfg.NodeID != "" {
		resource = append(resource, StrAttr("aura.node_id", x.cfg.NodeID))
	}
	body, err := json.Marshal(Document(resource, x.cfg.Version, spans))
	if err != nil {
		x.failed.Add(uint64(len(spans)))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.url, bytes.NewReader(body))
	if err != nil {
		x.failed.Add(uint64(len(spans)))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range x.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := x.cfg.Client.Do(req)
	if err != nil {
		x.fail(len(spans), err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		x.fail(len(spans), resp.Status)
		return
	}
	x.exported.Add(uint64(len(spans)))
	if x.failing.CompareAndSwap(true, false) {
		x.cfg.Log.Info("trace export recovered", "collector", x.url)
	}
}

// fail counts a refused batch and says so — once per run of failures rather
// than once per batch, so a collector that is down for an hour is one log
// line and a counter, not thirty-six hundred lines.
func (x *Exporter) fail(n int, why string) {
	x.failed.Add(uint64(n))
	if x.failing.CompareAndSwap(false, true) {
		x.cfg.Log.Warn("trace export failed; spans are being dropped, the causal log is unaffected",
			"collector", x.url, "reason", why)
	}
}

// ParseHeaders reads "k=v,k2=v2" — the form OTEL_EXPORTER_OTLP_HEADERS uses.
func ParseHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// boundedMap is a FIFO-evicting string map, touched by one goroutine.
type boundedMap struct {
	m     map[string]string
	order []string
	head  int
	max   int
}

func newBoundedMap(max int) *boundedMap {
	return &boundedMap{m: make(map[string]string, 1024), max: max}
}

func (b *boundedMap) get(k string) (string, bool) {
	v, ok := b.m[k]
	return v, ok
}

func (b *boundedMap) put(k, v string) {
	if _, ok := b.m[k]; ok {
		b.m[k] = v
		return
	}
	if len(b.m) >= b.max {
		delete(b.m, b.order[b.head])
		b.order[b.head] = k
		b.head = (b.head + 1) % b.max
	} else {
		b.order = append(b.order, k)
	}
	b.m[k] = v
}
