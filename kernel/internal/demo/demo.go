package demo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Options is what `aura demo` was asked for.
type Options struct {
	// Binary is the aura executable the nodes are started from.
	Binary string
	// Version is printed on the screen, so a recording says what it recorded.
	Version string
	// NodePort is where the node — and after the kill, its standby — listens.
	NodePort int
	// Sessions and Ops are how many relay and operation sessions run.
	Sessions int
	Ops      int
	// Rate is messages per second per relay session.
	Rate float64
	// EngineURL names a real OpenAI-compatible engine. Empty runs the
	// simulated one. EngineKey and EngineModel go with it.
	EngineURL   string
	EngineKey   string
	EngineModel string
	// EnginePriority says how a real engine wants priorities written: "vllm"
	// or "sglang". Empty writes none, which is the only safe thing to send an
	// engine that was not started to schedule by priority.
	EnginePriority string
	// Pace is the quiet stretch before each beat; Warmup the stretch before
	// the first, once every session is open.
	Pace   time.Duration
	Warmup time.Duration
	// Hold keeps the run from starting until Begin is called — the screen
	// calls it when someone is watching.
	Hold bool
	// Keep leaves the working directory behind, logs and ledger included.
	Keep bool
	// Out receives one line per thing that happens.
	Out io.Writer
}

func (o *Options) defaults() {
	if o.NodePort == 0 {
		o.NodePort = 7711
	}
	if o.Sessions <= 0 {
		o.Sessions = 300
	}
	if o.Ops < 0 {
		o.Ops = 0
	} else if o.Ops == 0 {
		o.Ops = 24
	}
	if o.Rate <= 0 {
		o.Rate = 10
	}
	if o.Pace <= 0 {
		o.Pace = 6 * time.Second
	}
	if o.Warmup <= 0 {
		o.Warmup = 5 * time.Second
	}
	if o.EngineModel == "" {
		o.EngineModel = "simulated"
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
}

// ── what the screen is shown ────────────────────────────────────────

// Snapshot is the whole state of a run at one instant. It carries numbers and
// identifiers only; every word on the screen belongs to the screen.
type Snapshot struct {
	Phase   string  `json:"phase"` // starting | waiting | running | done | failed
	Elapsed float64 `json:"elapsed"`
	Meta    Meta    `json:"meta"`
	Live    Live    `json:"live"`
	Series  []Point `json:"series"`
	Dots    string  `json:"dots"`
	Beats   []Beat  `json:"beats"`
	Error   string  `json:"error,omitempty"`
}

// Meta is what does not change during a run.
type Meta struct {
	Version  string  `json:"version"`
	OS       string  `json:"os"`
	Arch     string  `json:"arch"`
	CPUs     int     `json:"cpus"`
	Engine   string  `json:"engine"` // "simulated", or the real engine's host
	Sessions int     `json:"sessions"`
	Ops      int     `json:"ops"`
	Rate     float64 `json:"rate"`
	Node     string  `json:"node"`
}

// Live is the counters, as of the last sample.
type Live struct {
	// Node is who is answering: leader, down, or standby.
	Node string `json:"node"`
	// Sessions is the node's own count of live sessions.
	Sessions int `json:"sessions"`
	// Flowing is how many of the demo's sessions are answering, counted by
	// the sessions themselves.
	Flowing int `json:"flowing"`
	// Rate, P50 and P99 are relay round trips over the last second.
	Rate float64 `json:"rate"`
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
	// Tokens is what the node has counted passing through it to callers;
	// TokensRate is that per second.
	Tokens     uint64  `json:"tokens"`
	TokensRate float64 `json:"tokens_rate"`
	// Effects is entries in the ledger.
	Effects int64 `json:"effects"`
	// Executed and Rejected are the service's own counts.
	Executed int64 `json:"executed"`
	Rejected int64 `json:"rejected"`
	// Held is envelopes the node is holding for sessions that are behind.
	Held int64 `json:"held"`
	// Generating is model calls in flight at the node.
	Generating int `json:"generating"`
}

// Point is one tick of the plotted series.
type Point struct {
	T    float64 `json:"t"`
	Rate float64 `json:"rate"`
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
}

// Beat is one thing done to the system, and what was measured.
type Beat struct {
	ID     string         `json:"id"`
	Status string         `json:"status"` // pending | running | survived | failed
	Start  float64        `json:"start,omitempty"`
	End    float64        `json:"end,omitempty"`
	Facts  map[string]any `json:"facts,omitempty"`
	Checks []Check        `json:"checks,omitempty"`
}

// Check is one thing a beat required to be true.
type Check struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

var beatOrder = []string{"slow", "agent", "late", "cut", "kill", "tamper"}

// ── a run ───────────────────────────────────────────────────────────

// Run is one execution of the demo.
type Run struct {
	opt Options

	c     *cluster
	eng   *simEngine // nil when a real engine is used
	svc   *service
	cast  *cast
	lanes *lanes

	began   time.Time
	ended   time.Time // set when the run is over; the clock stops there
	begin   chan struct{}
	beginMu sync.Once

	mu       sync.Mutex
	phase    string
	meta     Meta
	live     Live
	series   []Point
	beats    []Beat
	failure  string
	nodeRole string
	lastDots string // the sessions as they were when the run ended

	tokenBase uint64 // tokens counted by nodes that are gone
	tokenLast uint64
	tokenAt   time.Time

	stopSampling chan struct{}
	stopOnce     sync.Once
	sampling     sync.WaitGroup
	closed       atomic.Bool
}

// New prepares a run. Nothing is started until Start.
func New(opt Options) *Run {
	opt.defaults()
	r := &Run{opt: opt, phase: "starting", begin: make(chan struct{}),
		nodeRole: "leader", stopSampling: make(chan struct{})}
	for _, id := range beatOrder {
		r.beats = append(r.beats, Beat{ID: id, Status: "pending", Facts: plan(id, opt.Sessions)})
	}
	engine := "simulated"
	if opt.EngineURL != "" {
		engine = opt.EngineURL
		if u, err := url.Parse(opt.EngineURL); err == nil && u.Host != "" {
			engine = u.Host
		}
	}
	r.meta = Meta{Version: opt.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), Engine: engine,
		Sessions: opt.Sessions, Ops: opt.Ops, Rate: opt.Rate}
	return r
}

// Begin releases a held run. Safe to call more than once.
func (r *Run) Begin() { r.beginMu.Do(func() { close(r.begin) }) }

func (r *Run) say(format string, args ...any) {
	fmt.Fprintf(r.opt.Out, format+"\n", args...)
}

func (r *Run) elapsed() float64 {
	switch {
	case r.began.IsZero():
		return 0
	case !r.ended.IsZero():
		return r.ended.Sub(r.began).Seconds()
	}
	return time.Since(r.began).Seconds()
}

// Snapshot returns the state right now.
func (r *Run) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{Phase: r.phase, Elapsed: r.elapsed(), Meta: r.meta, Live: r.live,
		Error: r.failure}
	s.Series = append([]Point(nil), r.series...)
	s.Beats = make([]Beat, len(r.beats))
	for i, b := range r.beats {
		s.Beats[i] = b
		s.Beats[i].Facts = make(map[string]any, len(b.Facts))
		for k, v := range b.Facts {
			s.Beats[i].Facts[k] = v
		}
		s.Beats[i].Checks = append([]Check(nil), b.Checks...)
	}
	switch {
	case r.lastDots != "":
		s.Dots = r.lastDots
	case r.lanes != nil:
		s.Dots = r.lanes.dots()
	}
	return s
}

func (r *Run) setPhase(p string) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

// Survived reports whether every beat ran and held.
func (s Snapshot) Survived() bool {
	if s.Phase != "done" {
		return false
	}
	for _, b := range s.Beats {
		if b.Status != "survived" {
			return false
		}
	}
	return true
}

// Start runs the whole demo and returns when it is over. The run's processes
// are stopped by Close, not here, so the final state stays readable.
func (r *Run) Start(ctx context.Context) error {
	err := r.run(ctx)
	r.freeze()
	r.mu.Lock()
	if err != nil {
		r.phase, r.failure = "failed", err.Error()
	} else {
		r.phase = "done"
	}
	r.mu.Unlock()
	return err
}

// freeze stops the clock and the sampling, so that what stays on the screen is
// the run as it ended rather than whatever the processes do while being left
// alone or shut down.
func (r *Run) freeze() {
	r.stopOnce.Do(func() { close(r.stopSampling) })
	r.sampling.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.began.IsZero() && r.ended.IsZero() {
		r.ended = time.Now()
	}
	if r.lanes != nil {
		r.lastDots = r.lanes.dots()
	}
}

func (r *Run) run(ctx context.Context) error {
	if err := r.setup(); err != nil {
		return err
	}
	if r.opt.Hold {
		r.setPhase("waiting")
		select {
		case <-r.begin:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	r.began = time.Now()
	r.setPhase("running")
	r.sampling.Add(1)
	go r.sample()

	ramp := 3 * time.Second
	r.lanes.start(ramp)
	r.say("opening %d sessions (%d relay at %.0f msg/s each, %d operations)",
		r.opt.Sessions+r.opt.Ops, r.opt.Sessions, r.opt.Rate, r.opt.Ops)
	if !r.waitFlowing(ramp + 20*time.Second) {
		relay, ops := r.lanes.flowing()
		return fmt.Errorf("only %d of %d relay and %d of %d operation sessions came up — %s",
			relay, r.opt.Sessions, ops, r.opt.Ops, r.diagnose())
	}
	if !sleep(ctx, r.opt.Warmup) {
		return ctx.Err()
	}
	w := r.lanes.rec.between(time.Now().Add(-2*time.Second), time.Now())
	r.say("steady: %.0f round-trips/s, p50 %.2f ms, p99 %.2f ms; %d operations carried out",
		w.Rate, w.P50, w.P99, r.lanes.opsDone.Load())
	// The beats assume the whole path works: model call, order, sealed effect,
	// credential, service. If it does not, saying so now is better than six
	// beats failing for a reason that is none of theirs.
	if r.opt.Ops > 0 && r.lanes.opsDone.Load() == 0 {
		return fmt.Errorf("no operation completed in the warm-up — %s", r.diagnose())
	}

	steps := map[string]func(*beat){
		"slow": r.beatSlow, "agent": r.beatAgent, "late": r.beatLate,
		"cut": r.beatCut, "kill": r.beatKill, "tamper": r.beatTamper,
	}
	for i, id := range beatOrder {
		if i > 0 && !sleep(ctx, r.opt.Pace) {
			return ctx.Err()
		}
		r.runBeat(i, steps[id])
	}
	if !sleep(ctx, r.opt.Pace/2) {
		return ctx.Err()
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// setup starts everything the run needs and waits for it to be ready.
func (r *Run) setup() error {
	if r.opt.Binary == "" {
		return errors.New("no aura binary to start nodes from")
	}
	var err error
	if r.svc, err = newService(); err != nil {
		return err
	}
	engineURL, engineKey, enginePriority := r.opt.EngineURL, r.opt.EngineKey, r.opt.EnginePriority
	if engineURL == "" {
		engineKey, enginePriority = newKey(), "vllm"
		if r.eng, err = newSimEngine(engineKey); err != nil {
			return err
		}
		engineURL = r.eng.url()
	}
	sessions := r.opt.Sessions + r.opt.Ops
	if r.c, err = newCluster(r.opt.Binary, r.opt.NodePort, engineURL, engineKey, enginePriority,
		max(1000, sessions*2+200)); err != nil {
		return err
	}
	r.say("starting a node and its standby on port %d (data: %s)", r.opt.NodePort, r.c.dir)
	if err := r.c.start(r.svc.key); err != nil {
		return err
	}
	r.mu.Lock()
	r.meta.Node = r.c.nodeID
	r.mu.Unlock()

	for _, g := range graphs() {
		if err := r.c.registerGraph(g); err != nil {
			return err
		}
	}
	tokens := map[string]string{}
	for _, capability := range []string{capRelay, capBrain, capPlanner, capActuator, capPayments} {
		tok, err := r.c.issueToken(capability)
		if err != nil {
			return err
		}
		tokens[capability] = tok
	}
	relays := max(2, min(8, r.opt.Sessions/60))
	r.cast = r.c.cast(r.svc, r.opt.EngineModel, tokens, relays, 3)
	if !r.cast.waitConnected(15 * time.Second) {
		return fmt.Errorf("the Expertise did not all connect — %s", r.diagnose())
	}
	r.lanes = r.c.lanes(r.opt.Sessions, r.opt.Ops, r.opt.Rate)
	return nil
}

var issuedToken = regexp.MustCompile(`(?m)^\s{2}(\S{20,})\s*$`)

// issueToken asks the node's own CLI for a credential that can register as one
// capability and nothing else, the way an operator provisions an Expertise.
func (c *cluster) issueToken(capability string) (string, error) {
	out, err := exec.Command(c.binary, "token", "issue", "--data", c.dir,
		"--capability", capability).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("issue a token for %s: %v\n%s", capability, err, out)
	}
	m := issuedToken.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("could not read the token issued for %s from:\n%s", capability, out)
	}
	return string(m[1]), nil
}

// diagnose gathers what would explain a failure to start.
func (r *Run) diagnose() string {
	why := ""
	if r.cast != nil {
		for _, v := range []*atomic.Value{&r.cast.inferErr, &r.cast.actErr} {
			if s := loadString(v); s != "" {
				why += s + "; "
			}
		}
	}
	return why + "last node output:\n" + r.c.tailLog(12)
}

func (r *Run) waitFlowing(limit time.Duration) bool {
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		relay, ops := r.lanes.flowing()
		if relay == len(r.lanes.relay) && ops == len(r.lanes.ops) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Close stops every process the run started.
func (r *Run) Close() {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}
	r.freeze()
	if r.lanes != nil {
		r.lanes.close()
	}
	if r.cast != nil {
		r.cast.close()
	}
	if r.c != nil {
		r.c.close(r.opt.Keep)
		if r.opt.Keep {
			r.say("kept: %s", r.c.root())
		}
	}
	if r.eng != nil {
		r.eng.close()
	}
	if r.svc != nil {
		r.svc.close()
	}
}

// ── sampling ────────────────────────────────────────────────────────

// nodeStats is the part of GET /v1/stats the demo reads.
type nodeStats struct {
	Sessions int `json:"sessions_live"`
	Mailbox  *struct {
		Queued int64  `json:"queued"`
		Waits  uint64 `json:"waits"`
	} `json:"mailbox"`
	Deadlines *struct {
		Dropped        uint64 `json:"dropped"`
		EffectsExpired uint64 `json:"effects_expired"`
	} `json:"deadlines"`
	Inference *struct {
		InFlight int               `json:"in_flight"`
		Stopped  map[string]uint64 `json:"stopped"`
		Graphs   []struct {
			Graph        string `json:"graph"`
			InFlight     int64  `json:"in_flight"`
			OutputTokens uint64 `json:"output_tokens"`
		} `json:"graphs"`
	} `json:"inference"`
}

func (s nodeStats) outputTokens() (n uint64) {
	if s.Inference != nil {
		for _, g := range s.Inference.Graphs {
			n += g.OutputTokens
		}
	}
	return n
}

func (s nodeStats) held() int64 {
	if s.Mailbox == nil {
		return 0
	}
	return s.Mailbox.Queued
}

func (s nodeStats) waits() uint64 {
	if s.Mailbox == nil {
		return 0
	}
	return s.Mailbox.Waits
}

func (s nodeStats) stopped(kind string) uint64 {
	if s.Inference == nil {
		return 0
	}
	return s.Inference.Stopped[kind]
}

// stats reads the node's own counters. ok is false when it did not answer.
func (c *cluster) stats() (nodeStats, bool) {
	var s nodeStats
	code, err := c.call(http.MethodGet, "/v1/stats", nil, &s)
	return s, err == nil && code == 200
}

// ledgerEntries reads how many effects the node has sealed, from /healthz.
func (c *cluster) ledgerEntries() (int64, bool) {
	var h struct {
		Ledger struct {
			Entries int64 `json:"entries"`
		} `json:"ledger"`
	}
	code, err := c.call(http.MethodGet, "/healthz", nil, &h)
	return h.Ledger.Entries, err == nil && code == 200
}

// sample keeps the plotted series and the counters current.
func (r *Run) sample() {
	defer r.sampling.Done()
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-r.stopSampling:
			return
		case now := <-t.C:
			n++
			tk := r.lanes.rec.roll(now)
			r.lanes.opsRec.roll(now)
			p := Point{T: r.elapsed(),
				Rate: float64(len(tk.lat)) / tickEvery.Seconds(),
				P50:  percentile(tk.lat, 0.50), P99: percentile(tk.lat, 0.99)}
			w := r.lanes.rec.between(now.Add(-time.Second), now)
			relay, ops := r.lanes.flowing()

			r.mu.Lock()
			r.series = append(r.series, p)
			r.live.Rate, r.live.P50, r.live.P99 = w.Rate, w.P50, w.P99
			r.live.Flowing = relay + ops
			r.live.Executed = r.svc.executed.Load()
			r.live.Rejected = r.svc.rejected.Load()
			r.mu.Unlock()

			if n%2 == 0 { // the node itself, twice a second
				r.pollNode(now)
			}
		}
	}
}

func (r *Run) pollNode(now time.Time) {
	st, ok := r.c.stats()
	entries, ok2 := r.c.ledgerEntries()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ok || !ok2 {
		r.live.Node = "down"
		r.live.TokensRate = 0
		return
	}
	r.live.Node = r.nodeRole
	r.live.Sessions = st.Sessions
	r.live.Held = st.held()
	r.live.Effects = entries
	if st.Inference != nil {
		r.live.Generating = st.Inference.InFlight
	}
	// A node's token count is in memory, so the standby starts from zero.
	// What the node before it counted is kept, so the total is of the run.
	cur := st.outputTokens()
	if cur < r.tokenLast {
		r.tokenBase += r.tokenLast
		r.tokenLast = 0
	}
	if !r.tokenAt.IsZero() {
		if dt := now.Sub(r.tokenAt).Seconds(); dt > 0 {
			r.live.TokensRate = float64(cur-r.tokenLast) / dt
		}
	}
	r.tokenLast, r.tokenAt = cur, now
	r.live.Tokens = r.tokenBase + cur
}

// ── beats ───────────────────────────────────────────────────────────

// beat is the handle a beat writes its findings through.
type beat struct {
	r *Run
	i int
}

func (b *beat) fact(key string, value any) {
	b.r.mu.Lock()
	if b.r.beats[b.i].Facts == nil {
		b.r.beats[b.i].Facts = map[string]any{}
	}
	b.r.beats[b.i].Facts[key] = value
	b.r.mu.Unlock()
}

func (b *beat) check(id string, ok bool, detail string, args ...any) bool {
	if ok {
		detail = ""
	} else if len(args) > 0 {
		detail = fmt.Sprintf(detail, args...)
	}
	b.r.mu.Lock()
	b.r.beats[b.i].Checks = append(b.r.beats[b.i].Checks, Check{ID: id, OK: ok, Detail: detail})
	b.r.mu.Unlock()
	mark := "ok  "
	if !ok {
		mark = "FAIL"
	}
	b.r.say("    %s %s%s", mark, id, map[bool]string{true: "", false: " — " + detail}[ok])
	return ok
}

func (r *Run) runBeat(i int, step func(*beat)) {
	r.mu.Lock()
	r.beats[i].Status, r.beats[i].Start = "running", r.elapsed()
	id := r.beats[i].ID
	r.mu.Unlock()
	r.say("\n[%d/%d] %s", i+1, len(beatOrder), id)

	b := &beat{r: r, i: i}
	func() {
		defer func() {
			if p := recover(); p != nil {
				b.check("ran", false, "the beat itself broke: %v", p)
			}
		}()
		step(b)
	}()

	r.mu.Lock()
	status := "survived"
	if len(r.beats[i].Checks) == 0 {
		status = "failed"
	}
	for _, c := range r.beats[i].Checks {
		if !c.OK {
			status = "failed"
		}
	}
	r.beats[i].Status, r.beats[i].End = status, r.elapsed()
	facts := r.beats[i].Facts
	r.mu.Unlock()
	r.say("    %s  %v", status, facts)
}

func round(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

func ms(d time.Duration) float64 { return round(float64(d.Microseconds())/1000, 1) }

// executable is the path nodes are started from when none was given.
func Executable() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}
