package infer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// What the plane counts. All of it is what the node itself saw pass through —
// never what an Expertise reported — which is the reason to keep it apart
// from the token meter the executor fills from attestations: the two answer
// different questions, and adding them together would answer neither.

// maxGraphs bounds how many graphs get their own row. A node serves a handful
// of graphs; this exists so that a caller inventing graph ids cannot grow the
// node's memory, or the cardinality of what it exports, without limit.
const maxGraphs = 256

// Labels for the two rows that are not a graph.
const (
	// Unattributed is requests that named no delivery: background work, or a
	// caller built before deliveries could be named.
	Unattributed = "(unattributed)"
	// OtherGraphs is where requests go once maxGraphs rows exist.
	OtherGraphs = "(other)"
)

// firstTokenBounds are the histogram's upper bounds, in milliseconds. Chosen
// around what a person notices: under a tenth of a second is immediate, a
// second is a pause, ten is a hang.
var firstTokenBounds = []int64{25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

type histogram struct {
	buckets [10]atomic.Uint64 // one per bound, and a last one for everything above
	count   atomic.Uint64
	sumMS   atomic.Uint64
}

func (h *histogram) observe(d time.Duration) {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	i := sort.Search(len(firstTokenBounds), func(i int) bool { return ms <= firstTokenBounds[i] })
	h.buckets[i].Add(1)
	h.count.Add(1)
	h.sumMS.Add(uint64(ms))
}

// Histogram is a latency distribution as counted, not as summarised: a
// percentile computed here would be a claim about the gaps between buckets.
type Histogram struct {
	// BoundsMS are the upper bounds of Buckets[0..n-1]; the last bucket has
	// no bound.
	BoundsMS []int64  `json:"bounds_ms"`
	Buckets  []uint64 `json:"buckets"`
	Count    uint64   `json:"count"`
	SumMS    uint64   `json:"sum_ms"`
}

func (h *histogram) snapshot() Histogram {
	out := Histogram{BoundsMS: firstTokenBounds, Buckets: make([]uint64, len(h.buckets)),
		Count: h.count.Load(), SumMS: h.sumMS.Load()}
	for i := range h.buckets {
		out.Buckets[i] = h.buckets[i].Load()
	}
	return out
}

type graphCounters struct {
	requests, completed, cancelled, failed atomic.Uint64
	metered, input, output, cached         atomic.Uint64
	inflight                               atomic.Int64
	firstToken                             histogram
}

// finish records how a request ended. kind is why the node or the caller
// ended it early, or "" when neither did.
func (g *graphCounters) finish(kind string, completed bool) {
	switch {
	case kind != "":
		g.cancelled.Add(1)
	case completed:
		g.completed.Add(1)
	default:
		g.failed.Add(1)
	}
}

type counters struct {
	mu     sync.RWMutex
	graphs map[string]*graphCounters

	refused       atomic.Uint64
	prepared      atomic.Uint64
	prepareMicros atomic.Uint64

	stopMu sync.Mutex
	stops  map[string]uint64
}

func (c *counters) graph(name string) *graphCounters {
	if name == "" {
		name = Unattributed
	}
	c.mu.RLock()
	g := c.graphs[name]
	c.mu.RUnlock()
	if g != nil {
		return g
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.graphs == nil {
		c.graphs = map[string]*graphCounters{}
	}
	if g = c.graphs[name]; g != nil {
		return g
	}
	if len(c.graphs) >= maxGraphs {
		name = OtherGraphs
		if g = c.graphs[name]; g != nil {
			return g
		}
	}
	g = &graphCounters{}
	c.graphs[name] = g
	return g
}

func (c *counters) stopped(kind string) {
	if kind == "" {
		return
	}
	c.stopMu.Lock()
	if c.stops == nil {
		c.stops = map[string]uint64{}
	}
	c.stops[kind]++
	c.stopMu.Unlock()
}

// GraphStats is one graph's inference, as the node observed it.
type GraphStats struct {
	Graph    string `json:"graph"`
	InFlight int64  `json:"in_flight"`
	// Requests is everything that reached the engine for this graph. It is
	// Completed + Cancelled + Failed + InFlight.
	Requests  uint64 `json:"requests"`
	Completed uint64 `json:"completed"`
	// Cancelled is requests the node or the caller ended before the engine
	// did: a cancelled chain, an ended session, a deadline, a caller that left.
	Cancelled uint64 `json:"cancelled"`
	// Failed is requests the engine refused or dropped.
	Failed uint64 `json:"failed"`
	// Metered is how many requests the engine reported usage for. The token
	// counts below are a floor wherever this is less than Completed.
	Metered           uint64 `json:"metered"`
	InputTokens       uint64 `json:"input_tokens"`
	OutputTokens      uint64 `json:"output_tokens"`
	CachedInputTokens uint64 `json:"cached_input_tokens"`
	// FirstToken is the time from the node sending a request to the engine
	// producing its first output, queueing at the engine included.
	FirstToken Histogram `json:"first_token"`
}

// Stats is what the plane reports about itself.
type Stats struct {
	// Upstream is the engine's base URL, credentials removed.
	Upstream string `json:"upstream"`
	// Priority is the convention the node writes priorities in, or empty when
	// it writes none.
	Priority string `json:"priority,omitempty"`
	InFlight int    `json:"in_flight"`
	// Refused is requests turned away before reaching the engine: a delivery
	// made to someone else, a chain already cancelled, a deadline already past.
	Refused uint64 `json:"refused"`
	// PrepareMicrosAvg is the mean time the node spent on a request before
	// sending it — reading it, placing it, editing it. The node's own share of
	// the latency, as opposed to the engine's.
	PrepareMicrosAvg float64 `json:"prepare_micros_avg"`
	// Stopped counts requests ended early, by why.
	Stopped map[string]uint64 `json:"stopped,omitempty"`
	// UsageRequested is false once the engine has refused `stream_options`:
	// from then on usage is read only where the engine offers it unasked.
	UsageRequested bool         `json:"usage_requested"`
	Graphs         []GraphStats `json:"graphs"`
	Engine         *EngineInfo  `json:"engine,omitempty"`
}

// Stats snapshots the plane.
func (p *Plane) Stats() Stats {
	out := Stats{
		Upstream: p.Upstream(), Priority: p.priorityName, InFlight: p.InFlight(),
		Refused: p.stats.refused.Load(), UsageRequested: p.usageSupported.Load(),
		Graphs: []GraphStats{}, Engine: p.engine.get(),
	}
	if n := p.stats.prepared.Load(); n > 0 {
		out.PrepareMicrosAvg = float64(p.stats.prepareMicros.Load()) / float64(n)
	}
	p.stats.stopMu.Lock()
	if len(p.stats.stops) > 0 {
		out.Stopped = make(map[string]uint64, len(p.stats.stops))
		for k, v := range p.stats.stops {
			out.Stopped[k] = v
		}
	}
	p.stats.stopMu.Unlock()

	p.stats.mu.RLock()
	for name, g := range p.stats.graphs {
		out.Graphs = append(out.Graphs, GraphStats{
			Graph: name, InFlight: g.inflight.Load(),
			Requests: g.requests.Load(), Completed: g.completed.Load(),
			Cancelled: g.cancelled.Load(), Failed: g.failed.Load(),
			Metered: g.metered.Load(), InputTokens: g.input.Load(),
			OutputTokens: g.output.Load(), CachedInputTokens: g.cached.Load(),
			FirstToken: g.firstToken.snapshot(),
		})
	}
	p.stats.mu.RUnlock()
	sort.Slice(out.Graphs, func(i, j int) bool { return out.Graphs[i].Graph < out.Graphs[j].Graph })
	return out
}

// EngineInfo is what the engine says about itself. In this phase the node
// did not start the engine and cannot vouch for any of it: these are the
// engine's own words, relayed.
type EngineInfo struct {
	Reachable bool `json:"reachable"`
	// Models is the ids the engine lists at /models.
	Models    []string `json:"models,omitempty"`
	CheckedAt int64    `json:"checked_at"` // unix millis
	Error     string   `json:"error,omitempty"`
}

type engineCache struct {
	mu       sync.Mutex
	info     *EngineInfo
	checking bool
}

func (c *engineCache) get() *EngineInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// engineRefresh is how stale the engine's self-description may get.
const engineRefresh = 30 * time.Second

// Probe asks the engine what it is serving. Bounded, and never called on a
// request's path: a node whose engine is down must still answer /v1/stats.
func (p *Plane) Probe(ctx context.Context) *EngineInfo {
	info := &EngineInfo{CheckedAt: p.now().UnixMilli()}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.upstream.String()+"/models", nil)
	if err == nil {
		if p.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		var resp *http.Response
		if resp, err = p.client.Do(req); err == nil {
			defer resp.Body.Close()
			var listed struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			info.Reachable = resp.StatusCode < 500
			if json.Unmarshal(raw, &listed) == nil {
				for _, m := range listed.Data {
					if m.ID != "" {
						info.Models = append(info.Models, m.ID)
					}
				}
			}
		}
	}
	if err != nil {
		info.Error = "unreachable"
	}
	p.engine.mu.Lock()
	p.engine.info = info
	p.engine.mu.Unlock()
	return info
}

// RefreshEngine re-probes in the background when what the node knows about
// its engine has gone stale. Returns at once.
func (p *Plane) RefreshEngine() {
	p.engine.mu.Lock()
	stale := p.engine.info == nil ||
		p.now().UnixMilli()-p.engine.info.CheckedAt > engineRefresh.Milliseconds()
	if !stale || p.engine.checking {
		p.engine.mu.Unlock()
		return
	}
	p.engine.checking = true
	p.engine.mu.Unlock()
	go p.Guard("engine probe", func() {
		defer func() {
			p.engine.mu.Lock()
			p.engine.checking = false
			p.engine.mu.Unlock()
		}()
		p.Probe(context.Background())
	})
}
