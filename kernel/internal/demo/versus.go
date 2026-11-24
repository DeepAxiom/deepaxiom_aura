package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

// The comparison: the same work, the same four things done to it at the same
// instant, once on an ordinary agent and once on a node.
//
// Both sides carry the same load — sessions that ask for an operation, which
// is a model call, an order carried out at a service, and an answer — against
// the same simulated engine. They differ in what is between the client and the
// service: on one side the single process in plain.go, on the other `aura up`
// with a standby.
//
// What is done to both:
//
//	taken   the part that talks to the model is taken over, and tries to have
//	        orders carried out that neither system is allowed to carry out
//	late    orders are issued after the decision behind them stopped being valid
//	crash   SIGKILL, and then every client that never got its answer asks again
//	forged  one character of the record of what was done is changed on disk
//
// Each side is asked the same question afterwards and the answer is counted at
// the service, not taken from either system's own account of itself.

const (
	sidePlain = 0
	sideAura  = 1
)

// VSnapshot is the state of a comparison at one instant.
type VSnapshot struct {
	Phase   string   `json:"phase"`
	Elapsed float64  `json:"elapsed"`
	Meta    Meta     `json:"meta"`
	Plain   Side     `json:"plain"`
	Aura    Side     `json:"aura"`
	Series  []VPoint `json:"series"`
	Beats   []VBeat  `json:"beats"`
	Error   string   `json:"error,omitempty"`
}

// Side is one of the two systems, as of the last sample.
type Side struct {
	// State is up, down or restarted for the agent; leader, down or standby
	// for the node.
	State string `json:"state"`
	// Sessions is how many of this side's sessions are being answered.
	Sessions int `json:"sessions"`
	// Executed is orders its service has carried out, by the service's count.
	Executed int64 `json:"executed"`
	// Rate is operations answered per second.
	Rate float64 `json:"rate"`
	Dots string  `json:"dots"`
	// Harm is how many things have happened on this side that should not have.
	Harm int `json:"harm"`
}

// VPoint is one tick of operations answered per second, on each side.
type VPoint struct {
	T     float64 `json:"t"`
	Plain float64 `json:"plain"`
	Aura  float64 `json:"aura"`
}

// VBeat is one thing done to both systems.
type VBeat struct {
	ID     string         `json:"id"`
	Status string         `json:"status"` // pending | running | done
	Start  float64        `json:"start,omitempty"`
	End    float64        `json:"end,omitempty"`
	Plain  map[string]any `json:"plain,omitempty"`
	Aura   map[string]any `json:"aura,omitempty"`
	// Harmed says the agent was harmed, as the comparison says it would be.
	// Held says the node was not. Either can come out the other way, and is
	// shown if it does.
	Harmed bool    `json:"harmed"`
	Held   bool    `json:"held"`
	Checks []Check `json:"checks,omitempty"`
}

var versusOrder = []string{"taken", "late", "crash", "forged"}

// How many of each thing a beat does, fixed here so the screen can say what is
// about to happen to both sides before it does.
const (
	takenOrders   = 20
	crashRequests = 20
)

func versusPlan(id string) map[string]any {
	switch id {
	case "taken":
		return map[string]any{"orders": takenOrders}
	case "late":
		return map[string]any{"orders": lateEach, "delay_ms": lateDelay, "deadline_ms": lateDeadlineMS}
	case "crash":
		return map[string]any{"requests": crashRequests}
	}
	return map[string]any{}
}

// Versus is one run of the comparison.
type Versus struct {
	opt Options

	eng   *simEngine
	svc   [2]*service
	c     *cluster
	cast  *cast
	plain *plainProc
	flows [2][]*flow

	answered [2]atomic.Int64

	began time.Time
	ended time.Time
	begin chan struct{}
	once  sync.Once

	mu       sync.Mutex
	phase    string
	meta     Meta
	sides    [2]Side
	series   []VPoint
	beats    []VBeat
	failure  string
	auraRole string
	lastDots [2]string
	recent   [2][]int64 // answered counts at the last few ticks

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	sampling sync.WaitGroup
	closed   atomic.Bool
}

// NewVersus prepares a comparison. Sessions is per side.
func NewVersus(opt Options) *Versus {
	if opt.Sessions <= 0 {
		opt.Sessions = 60
	}
	opt.Ops = -1
	opt.defaults()
	if opt.PlainPort == 0 {
		opt.PlainPort = opt.NodePort + 1
	}
	v := &Versus{opt: opt, phase: "starting", begin: make(chan struct{}),
		auraRole: "leader", stop: make(chan struct{})}
	for _, id := range versusOrder {
		v.beats = append(v.beats, VBeat{ID: id, Status: "pending", Plain: versusPlan(id), Aura: versusPlan(id)})
	}
	v.meta = Meta{Version: opt.Version, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(),
		Engine: "simulated", Sessions: opt.Sessions}
	return v
}

func (v *Versus) Begin() { v.once.Do(func() { close(v.begin) }) }

func (v *Versus) say(format string, args ...any) { fmt.Fprintf(v.opt.Out, format+"\n", args...) }

func (v *Versus) stopped() bool {
	select {
	case <-v.stop:
		return true
	default:
		return false
	}
}

func (v *Versus) elapsed() float64 {
	switch {
	case v.began.IsZero():
		return 0
	case !v.ended.IsZero():
		return v.ended.Sub(v.began).Seconds()
	}
	return time.Since(v.began).Seconds()
}

func dotsOf(flows []*flow) string {
	var b strings.Builder
	for _, f := range flows {
		b.WriteByte(dotChars[f.state.Load()])
	}
	return b.String()
}

// Snapshot returns the state right now.
func (v *Versus) Snapshot() VSnapshot {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := VSnapshot{Phase: v.phase, Elapsed: v.elapsed(), Meta: v.meta,
		Plain: v.sides[sidePlain], Aura: v.sides[sideAura], Error: v.failure}
	s.Series = append([]VPoint(nil), v.series...)
	for side, dst := range []*Side{&s.Plain, &s.Aura} {
		if v.lastDots[side] != "" {
			dst.Dots = v.lastDots[side]
		} else {
			dst.Dots = dotsOf(v.flows[side])
		}
	}
	s.Beats = make([]VBeat, len(v.beats))
	for i, b := range v.beats {
		s.Beats[i] = b
		s.Beats[i].Plain, s.Beats[i].Aura = copyFacts(b.Plain), copyFacts(b.Aura)
		s.Beats[i].Checks = append([]Check(nil), b.Checks...)
	}
	return s
}

func copyFacts(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	return out
}

// State, Survived and Summary are what the command needs of any run.
func (v *Versus) State() any { return v.Snapshot() }

// Survived reports whether the run came out as the comparison is described:
// the node held through every beat, and the plain agent was harmed by each.
//
// The second half is not a wish. The documentation shows this comparison and
// says what happens on the left; if a change makes that stop being true, the
// honest outcome is a failing run, not a picture that no longer matches.
func (v *Versus) Survived() bool {
	s := v.Snapshot()
	if s.Phase != "done" {
		return false
	}
	for _, b := range s.Beats {
		if b.Status != "done" || !b.Held || !b.Harmed {
			return false
		}
	}
	return true
}

// Summary is the last line of a run.
func (v *Versus) Summary() string {
	s := v.Snapshot()
	held, harmed := 0, 0
	for _, b := range s.Beats {
		if b.Held {
			held++
		}
		if b.Harmed {
			harmed++
		}
	}
	return fmt.Sprintf("the node held %d of %d; the plain agent was harmed in %d of %d (%d things happened to it that should not have)",
		held, len(s.Beats), harmed, len(s.Beats), s.Plain.Harm)
}

// Start runs the comparison and returns when it is over.
func (v *Versus) Start(ctx context.Context) error {
	err := v.run(ctx)
	v.freeze()
	v.mu.Lock()
	if err != nil {
		v.phase, v.failure = "failed", err.Error()
	} else {
		v.phase = "done"
	}
	v.mu.Unlock()
	return err
}

func (v *Versus) freeze() {
	v.stopOnce.Do(func() { close(v.stop) })
	v.sampling.Wait()
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.began.IsZero() && v.ended.IsZero() {
		v.ended = time.Now()
	}
	for side := range v.flows {
		if len(v.flows[side]) > 0 {
			v.lastDots[side] = dotsOf(v.flows[side])
		}
	}
}

// Close stops everything the comparison started.
func (v *Versus) Close() {
	if !v.closed.CompareAndSwap(false, true) {
		return
	}
	v.freeze()
	for side := range v.flows {
		for _, f := range v.flows[side] {
			f.kick()
		}
	}
	done := make(chan struct{})
	go func() { v.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	if v.cast != nil {
		v.cast.close()
	}
	if v.plain != nil {
		v.plain.close()
	}
	if v.c != nil {
		v.c.close(v.opt.Keep)
		if v.opt.Keep {
			v.say("kept: %s", v.c.root())
		}
	}
	if v.eng != nil {
		v.eng.close()
	}
	for _, s := range v.svc {
		if s != nil {
			s.close()
		}
	}
}

func (v *Versus) run(ctx context.Context) error {
	if err := v.setup(); err != nil {
		return err
	}
	if v.opt.Hold {
		v.mu.Lock()
		v.phase = "waiting"
		v.mu.Unlock()
		select {
		case <-v.begin:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	v.began = time.Now()
	v.mu.Lock()
	v.phase = "running"
	v.mu.Unlock()
	v.sampling.Add(1)
	go v.sample()

	// The two sides open their sessions interleaved, so neither gets a head
	// start on the other.
	gap := 2 * time.Second / time.Duration(2*v.opt.Sessions)
	for i := 0; i < v.opt.Sessions; i++ {
		v.wg.Add(2)
		go v.runPlain(v.flows[sidePlain][i])
		time.Sleep(gap)
		go v.runAura(v.flows[sideAura][i])
		time.Sleep(gap)
	}
	v.say("opening %d sessions on each side", v.opt.Sessions)
	if !v.waitAnswering(20 * time.Second) {
		return fmt.Errorf("the sessions did not all come up — last node output:\n%s", v.c.tailLog(12))
	}
	if !sleep(ctx, v.opt.Warmup) {
		return ctx.Err()
	}
	p, a := v.answered[sidePlain].Load(), v.answered[sideAura].Load()
	v.say("steady: %d operations answered by the plain agent, %d through the node", p, a)
	if p == 0 || a == 0 {
		return fmt.Errorf("one side is not completing operations (plain %d, node %d) — %s; last node output:\n%s",
			p, a, loadString(&v.cast.inferErr)+" "+loadString(&v.cast.actErr), v.c.tailLog(12))
	}

	steps := map[string]func(*vbeat){
		"taken": v.beatTaken, "late": v.beatLate, "crash": v.beatCrash, "forged": v.beatForged,
	}
	for i, id := range versusOrder {
		if i > 0 && !sleep(ctx, v.opt.Pace) {
			return ctx.Err()
		}
		v.runBeat(i, steps[id])
	}
	if !sleep(ctx, v.opt.Pace/2) {
		return ctx.Err()
	}
	return nil
}

func (v *Versus) setup() error {
	if v.opt.Binary == "" {
		return errors.New("no aura binary to start the two systems from")
	}
	if v.opt.EngineURL != "" {
		return errors.New("the comparison runs on the simulated engine; --engine-url is for `aura demo` alone")
	}
	var err error
	for side := range v.svc {
		if v.svc[side], err = newService(); err != nil {
			return err
		}
	}
	engineKey := newKey()
	if v.eng, err = newSimEngine(engineKey); err != nil {
		return err
	}
	if v.c, err = newCluster(v.opt.Binary, v.opt.NodePort, v.eng.url(), engineKey, "vllm",
		max(1000, v.opt.Sessions*4+200)); err != nil {
		return err
	}
	v.say("starting a node and its standby on port %d, and a plain agent on port %d", v.opt.NodePort, v.opt.PlainPort)
	if err := v.c.start(v.svc[sideAura].key); err != nil {
		return err
	}
	v.mu.Lock()
	v.meta.Node = v.c.nodeID
	v.mu.Unlock()
	for _, g := range graphs() {
		if err := v.c.registerGraph(g); err != nil {
			return err
		}
	}
	tokens := map[string]string{}
	for _, capability := range []string{capRelay, capBrain, capPlanner, capActuator, capPayments, capScribe} {
		if tokens[capability], err = v.c.issueToken(capability); err != nil {
			return err
		}
	}
	v.cast = v.c.cast(v.svc[sideAura], "simulated", tokens, 1, 3)
	if !v.cast.waitConnected(15 * time.Second) {
		return fmt.Errorf("the Expertise did not all connect — last node output:\n%s", v.c.tailLog(12))
	}

	v.plain = &plainProc{binary: v.opt.Binary, logs: v.c.logs, cfg: PlainConfig{
		Listen:    fmt.Sprintf("127.0.0.1:%d", v.opt.PlainPort),
		EngineURL: v.eng.url(), EngineKey: engineKey,
		ServiceURL: v.svc[sidePlain].url(), ServiceKey: v.svc[sidePlain].key,
		LogPath: filepath.Join(v.c.root(), "plain-audit.log"),
	}}
	if err := v.plain.start(); err != nil {
		return err
	}
	if !v.plain.waitHealthy(10 * time.Second) {
		return errors.New("the plain agent did not start")
	}

	stamp := time.Now().UnixNano() % 1_000_000
	for i := 0; i < v.opt.Sessions; i++ {
		v.flows[sidePlain] = append(v.flows[sidePlain], &flow{id: fmt.Sprintf("demo-wp-%d-%03d", stamp, i)})
		v.flows[sideAura] = append(v.flows[sideAura], &flow{graph: graphWork, id: fmt.Sprintf("demo-wa-%d-%03d", stamp, i)})
	}
	return nil
}

func answering(flows []*flow) int {
	n := 0
	for _, f := range flows {
		if s := f.state.Load(); s == dotFlowing || s == dotForgot {
			n++
		}
	}
	return n
}

func (v *Versus) waitAnswering(limit time.Duration) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if answering(v.flows[sidePlain]) == v.opt.Sessions && answering(v.flows[sideAura]) == v.opt.Sessions {
			return true
		}
	}
	return false
}

func pause(stop <-chan struct{}) {
	select {
	case <-stop:
	case <-time.After(time.Duration(300+rand.Intn(300)) * time.Millisecond):
	}
}

// runPlain is one session against the agent: ask, be answered, ask again.
func (v *Versus) runPlain(f *flow) {
	defer v.wg.Done()
	n, wasDown := 0, false
	for !v.stopped() {
		conn, _, err := websocket.DefaultDialer.Dial(v.plain.ws()+"/session?id="+f.id, nil)
		if err != nil {
			if f.state.Load() != dotOpening {
				f.state.Store(dotDown)
			}
			time.Sleep(time.Duration(8+rand.Intn(24)) * time.Millisecond)
			continue
		}
		f.setPlain(conn)
		for !v.stopped() {
			n++
			order := fmt.Sprintf("%s-%d", f.id, n)
			if conn.WriteJSON(plainMsg{ID: order, Op: "work", Order: order}) != nil {
				break
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			var r plainReply
			if conn.ReadJSON(&r) != nil {
				break
			}
			// The agent says how far along this session is. If that number
			// has gone back, the agent answering is not the one that knew it.
			if before := f.turn.Load(); before > 0 && int64(r.Turn) <= before {
				f.forgot.Store(true)
			}
			f.turn.Store(int64(r.Turn))
			if f.forgot.Load() {
				f.state.Store(dotForgot)
			} else {
				f.state.Store(dotFlowing)
			}
			now := time.Now().UnixNano()
			v.answered[sidePlain].Add(1)
			f.lastReply.Store(now)
			if wasDown {
				wasDown = false
				f.backAt.Store(now)
			}
			pause(v.stop)
		}
		conn.Close()
		f.setPlain(nil)
		if v.stopped() {
			return
		}
		f.state.Store(dotDown)
		wasDown = true
	}
}

// runAura is the same session through the node.
func (v *Versus) runAura(f *flow) {
	defer v.wg.Done()
	n, wasDown := 0, false
	for !v.stopped() {
		cl, err := v.c.open(f.graph, f.id)
		if err != nil {
			if f.state.Load() != dotOpening {
				f.state.Store(dotDown)
			}
			time.Sleep(time.Duration(8+rand.Intn(24)) * time.Millisecond)
			continue
		}
		f.setConn(cl)
		for !v.stopped() {
			n++
			order := fmt.Sprintf("%s-%d", f.id, n)
			if _, err := cl.say("op " + order); err != nil {
				break
			}
			refused := false
			_, err := cl.await(10*time.Second, func(env envelope) bool {
				if env.Kind == channel.KindError {
					refused = true
					return true
				}
				return env.Kind == channel.KindData && textOf(env) == "done "+order
			})
			if err != nil || refused {
				break
			}
			f.state.Store(dotFlowing)
			now := time.Now().UnixNano()
			v.answered[sideAura].Add(1)
			f.lastReply.Store(now)
			if wasDown {
				wasDown = false
				f.backAt.Store(now)
			}
			pause(v.stop)
		}
		cl.close()
		f.setConn(nil)
		if v.stopped() {
			return
		}
		f.state.Store(dotDown)
		wasDown = true
	}
}

// sample keeps the two sides' counters and the plotted series current.
func (v *Versus) sample() {
	defer v.sampling.Done()
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	n := 0
	for {
		select {
		case <-v.stop:
			return
		case <-t.C:
			n++
			var rate [2]float64
			v.mu.Lock()
			for side := range v.recent {
				v.recent[side] = append(v.recent[side], v.answered[side].Load())
				if len(v.recent[side]) > 5 { // one second of ticks, plus the one before
					v.recent[side] = v.recent[side][1:]
				}
				r := v.recent[side]
				rate[side] = float64(r[len(r)-1]-r[0]) / (float64(len(r)-1) * tickEvery.Seconds())
				if len(r) < 2 {
					rate[side] = 0
				}
				v.sides[side].Rate = rate[side]
				v.sides[side].Sessions = answering(v.flows[side])
				v.sides[side].Executed = v.svc[side].executed.Load()
			}
			v.series = append(v.series, VPoint{T: v.elapsed(), Plain: rate[sidePlain], Aura: rate[sideAura]})
			v.mu.Unlock()

			if n%2 == 0 {
				_, nodeUp := v.c.ledgerEntries()
				plainUp := false
				if resp, err := probe.Get(v.plain.base() + "/healthz"); err == nil {
					resp.Body.Close()
					plainUp = resp.StatusCode == 200
				}
				v.mu.Lock()
				v.sides[sideAura].State = "down"
				if nodeUp {
					v.sides[sideAura].State = v.auraRole
				}
				v.sides[sidePlain].State = "down"
				if plainUp {
					v.sides[sidePlain].State = "up"
					if v.plain.starts.Load() > 1 {
						v.sides[sidePlain].State = "restarted"
					}
				}
				v.mu.Unlock()
			}
		}
	}
}

// ── the agent's process ─────────────────────────────────────────────

// plainProc runs the plain agent and, like any supervisor, starts it again
// when it dies.
type plainProc struct {
	binary string
	cfg    PlainConfig
	logs   string

	mu     sync.Mutex
	cmd    *exec.Cmd
	starts atomic.Int64
	closed atomic.Bool
}

func (p *plainProc) base() string { return "http://" + p.cfg.Listen }
func (p *plainProc) ws() string   { return "ws://" + p.cfg.Listen }

func (p *plainProc) start() error {
	logf, err := os.OpenFile(filepath.Join(p.logs, "plain-agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(p.binary, "demo-plain")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(os.Environ(), p.cfg.env()...)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("start the plain agent: %w", err)
	}
	p.mu.Lock()
	p.cmd = cmd
	p.mu.Unlock()
	p.starts.Add(1)
	go func() {
		_ = cmd.Wait()
		logf.Close()
		// Restart=always: the agent is given the same care in production that
		// any service is. What it comes back as is the question.
		for !p.closed.Load() {
			if p.start() == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return nil
}

func (p *plainProc) kill() error {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("there is no plain agent to kill")
	}
	return cmd.Process.Kill()
}

func (p *plainProc) waitHealthy(limit time.Duration) bool {
	probe := &http.Client{Timeout: 300 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if resp, err := probe.Get(p.base() + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
	}
	return false
}

func (p *plainProc) close() {
	p.closed.Store(true)
	_ = p.kill()
}

// call makes one JSON request to the agent.
func (p *plainProc) call(method, path string, body, out any) (int, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, p.base()+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// ── beats ───────────────────────────────────────────────────────────

type vbeat struct {
	v *Versus
	i int
}

func (b *vbeat) set(side int, key string, value any) {
	b.v.mu.Lock()
	beat := &b.v.beats[b.i]
	target := &beat.Plain
	if side == sideAura {
		target = &beat.Aura
	}
	if *target == nil {
		*target = map[string]any{}
	}
	(*target)[key] = value
	b.v.mu.Unlock()
}

func (b *vbeat) plain(key string, value any) { b.set(sidePlain, key, value) }
func (b *vbeat) aura(key string, value any)  { b.set(sideAura, key, value) }

// check records something that had to be true of the node.
func (b *vbeat) check(id string, ok bool, detail string, args ...any) bool {
	if ok {
		detail = ""
	} else if len(args) > 0 {
		detail = fmt.Sprintf(detail, args...)
	}
	b.v.mu.Lock()
	b.v.beats[b.i].Checks = append(b.v.beats[b.i].Checks, Check{ID: id, OK: ok, Detail: detail})
	b.v.mu.Unlock()
	if ok {
		b.v.say("    ok   %s", id)
	} else {
		b.v.say("    FAIL %s — %s", id, detail)
	}
	return ok
}

// harm adds to a side's count of things that should not have happened.
func (b *vbeat) harm(side, n int) {
	if n <= 0 {
		return
	}
	b.v.mu.Lock()
	b.v.sides[side].Harm += n
	b.v.mu.Unlock()
}

func (v *Versus) runBeat(i int, step func(*vbeat)) {
	v.mu.Lock()
	v.beats[i].Status, v.beats[i].Start = "running", v.elapsed()
	id := v.beats[i].ID
	harmBefore := [2]int{v.sides[sidePlain].Harm, v.sides[sideAura].Harm}
	v.mu.Unlock()
	v.say("\n[%d/%d] %s", i+1, len(versusOrder), id)

	b := &vbeat{v: v, i: i}
	func() {
		defer func() {
			if p := recover(); p != nil {
				b.check("ran", false, "the beat itself broke: %v", p)
			}
		}()
		step(b)
	}()

	v.mu.Lock()
	beat := &v.beats[i]
	beat.Held = len(beat.Checks) > 0 && v.sides[sideAura].Harm == harmBefore[sideAura]
	for _, c := range beat.Checks {
		if !c.OK {
			beat.Held = false
		}
	}
	beat.Harmed = v.sides[sidePlain].Harm > harmBefore[sidePlain]
	beat.Status, beat.End = "done", v.elapsed()
	plain, aura, harmed, held := beat.Plain, beat.Aura, beat.Harmed, beat.Held
	v.mu.Unlock()
	v.say("    plain agent: harmed=%v %v", harmed, plain)
	v.say("    node:        held=%v %v", held, aura)
}

// taken: the part that talks to the model is no longer the operator's.
func (v *Versus) beatTaken(b *vbeat) {
	const n = takenOrders
	stamp := time.Now().UnixNano() % 1_000_000
	orders := make([]string, n)
	for i := range orders {
		orders[i] = fmt.Sprintf("%s%d-%d", forbiddenPrefix, stamp, i)
	}
	b.plain("orders", n)
	b.aura("orders", n)

	// The agent, through its front door first: its own rule does hold there.
	refused := 0
	if conn, _, err := websocket.DefaultDialer.Dial(v.plain.ws()+"/session?id=demo-x-front", nil); err == nil {
		for _, order := range orders {
			_ = conn.WriteJSON(plainMsg{ID: "front-" + order, Op: "work", Order: order})
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var r plainReply
			if conn.ReadJSON(&r) == nil && r.Error != "" {
				refused++
			}
		}
		conn.Close()
	}
	// And then from inside it, where the rule is not on the way.
	_, _ = v.plain.call(http.MethodPost, "/taken-over", map[string]any{"orders": orders}, nil)

	// The node, with what the Expertise that was taken over holds: its token.
	token := v.cast.brains[0].token
	direct, broker, denied := 0, 0, 0
	for _, order := range orders {
		if orderDirect(v.c.http, v.svc[sideAura], order, "") == http.StatusUnauthorized {
			direct++
		}
		var out struct {
			Values map[string]string `json:"values"`
		}
		code, _ := v.c.callAs(token, http.MethodPost, "/v1/secrets/resolve", map[string]any{
			"receipt": "", "capability": capActuator, "names": []string{secretName},
		}, &out)
		if code == http.StatusForbidden && len(out.Values) == 0 {
			broker++
		}
		if ok, _ := v.c.runForbidden(order); !ok {
			denied++
		}
	}
	time.Sleep(300 * time.Millisecond)

	carried := [2]int{}
	for _, order := range orders {
		for side := range carried {
			if v.svc[side].carriedOut(order) {
				carried[side]++
			}
		}
	}
	handed := int(v.cast.handedToPayments.Load())

	b.plain("refused_at_the_front", refused)
	b.plain("carried_out", carried[sidePlain])
	b.aura("carried_out", carried[sideAura])
	b.aura("no_key", direct)
	b.aura("broker_refused", broker)
	b.aura("policy_denied", denied)
	b.harm(sidePlain, carried[sidePlain])
	b.harm(sideAura, carried[sideAura]+handed)

	b.check("the_node_carried_out_none", carried[sideAura] == 0 && handed == 0,
		"%d forbidden orders were carried out and %d were handed to the forbidden Expertise", carried[sideAura], handed)
	b.check("the_node_refused_every_way_in", direct == n && broker == n && denied == n,
		"refused: %d with no key, %d at the broker, %d by policy, of %d each", direct, broker, denied, n)
}

// late: orders issued after the decision behind them stopped being valid.
func (v *Versus) beatLate(b *vbeat) {
	const n = lateEach
	stamp := time.Now().UnixNano() % 1_000_000
	for _, side := range []int{sidePlain, sideAura} {
		b.set(side, "orders", n)
		b.set(side, "delay_ms", lateDelay)
		b.set(side, "deadline_ms", lateDeadlineMS)
	}
	plainOrder := func(i int) string { return fmt.Sprintf("late-p-%d-%d", stamp, i) }
	auraOrder := func(i int) string { return fmt.Sprintf("late-a-%d-%d", stamp, i) }
	auraSession := func(i int) string { return fmt.Sprintf("demo-l-%d-%d", stamp, i) }

	lateBy := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(v.plain.ws()+fmt.Sprintf("/session?id=demo-lp-%d-%d", stamp, i), nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteJSON(plainMsg{ID: plainOrder(i), Op: "late", Order: plainOrder(i), DelayMS: lateDelay})
			_ = conn.SetReadDeadline(time.Now().Add(time.Duration(lateDelay+3000) * time.Millisecond))
			var r plainReply
			_ = conn.ReadJSON(&r)
		}(i)
		go func(i int) {
			defer wg.Done()
			cl, err := v.c.open(graphLate, auraSession(i))
			if err != nil {
				return
			}
			defer cl.close()
			if _, err := cl.say(fmt.Sprintf("order %d %s", lateDelay, auraOrder(i))); err != nil {
				return
			}
			end := time.Now().Add(time.Duration(lateDelay+900) * time.Millisecond)
			for time.Now().Before(end) {
				env, err := cl.read(time.Until(end))
				if err != nil {
					return
				}
				if event, late := eventOf(env); env.Kind == channel.KindStatus && event == "deadline_exceeded" {
					lateBy[i] = late
					return
				}
			}
		}(i)
		time.Sleep(3 * time.Millisecond)
	}
	wg.Wait()

	carried, sealed, late := [2]int{}, 0, int64(0)
	for i := 0; i < n; i++ {
		if v.svc[sidePlain].carriedOut(plainOrder(i)) {
			carried[sidePlain]++
		}
		if v.svc[sideAura].carriedOut(auraOrder(i)) || v.cast.handed(auraOrder(i)) {
			carried[sideAura]++
		}
		var led struct {
			Entries []struct {
				Outcome string `json:"outcome"`
			} `json:"entries"`
		}
		v.c.call(http.MethodGet, "/v1/sessions/"+auraSession(i)+"/ledger", nil, &led)
		for _, e := range led.Entries {
			if e.Outcome == "expired" {
				sealed++
			}
		}
		late = max(late, lateBy[i])
	}
	b.plain("carried_out", carried[sidePlain])
	b.aura("carried_out", carried[sideAura])
	b.aura("sealed_expired", sealed)
	if late > 0 {
		b.aura("late_by_ms", late)
	}
	b.harm(sidePlain, carried[sidePlain])
	b.harm(sideAura, carried[sideAura])

	b.check("the_node_carried_out_no_late_order", carried[sideAura] == 0,
		"%d late orders were carried out", carried[sideAura])
	b.check("each_is_sealed_as_expired", sealed == n,
		"the ledger holds an expired entry for %d of %d late orders", sealed, n)
}

// crash: SIGKILL to both, and then the clients that never got an answer ask
// again — the only thing a client that does not know can do.
func (v *Versus) beatCrash(b *vbeat) {
	const n = crashRequests
	for end := time.Now().Add(8 * time.Second); !v.c.standbyWatching() && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.check("a_standby_was_watching", v.c.standbyWatching(), "the standby never said who it stood by for") {
		return
	}
	stamp := time.Now().UnixNano() % 1_000_000
	order := func(side, i int) string { return fmt.Sprintf("once-%d-%d-%d", side, stamp, i) }
	session := func(side, i int) string { return fmt.Sprintf("demo-q%d-%d-%d", side, stamp, i) }
	b.plain("requests", n)
	b.aura("requests", n)

	// Twenty requests on each side, sent and not yet answered: the order is
	// carried out, and the answer is still being put into words.
	plainConn := make([]*websocket.Conn, n)
	auraConn := make([]*client, n)
	var answered [2][n]atomic.Bool // which requests did get their answer
	for i := 0; i < n; i++ {
		if conn, _, err := websocket.DefaultDialer.Dial(v.plain.ws()+"/session?id="+session(sidePlain, i), nil); err == nil {
			plainConn[i] = conn
			_ = conn.WriteJSON(plainMsg{ID: order(sidePlain, i), Op: "work", Order: order(sidePlain, i)})
			go func(i int) {
				var r plainReply
				if conn.ReadJSON(&r) == nil && r.Done != "" {
					answered[sidePlain][i].Store(true)
				}
			}(i)
		}
		if cl, err := v.c.open(graphWork, session(sideAura, i)); err == nil {
			auraConn[i] = cl
			_ = cl.sayAs(order(sideAura, i), "op "+order(sideAura, i))
			go func(i int) {
				if _, err := cl.await(30*time.Second, func(env envelope) bool {
					return env.Kind == channel.KindData && textOf(env) == "done "+order(sideAura, i)
				}); err == nil {
					answered[sideAura][i].Store(true)
				}
			}(i)
		}
	}
	executed := func(side int) int {
		c := 0
		for i := 0; i < n; i++ {
			if v.svc[side].carriedOut(order(side, i)) {
				c++
			}
		}
		return c
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if executed(sidePlain) == n && executed(sideAura) == n {
			break
		}
	}
	if !b.check("the_orders_were_carried_out_before_the_crash", executed(sidePlain) == n && executed(sideAura) == n,
		"%d of %d on the agent and %d of %d on the node had been carried out", executed(sidePlain), n, executed(sideAura), n) {
		return
	}
	flows := [2][]*flow{v.flows[sidePlain], v.flows[sideAura]}

	// Both at once.
	killedAt := time.Now()
	_ = v.plain.kill()
	_, err := v.c.killLeader()
	if !b.check("both_were_killed", err == nil, "%v", err) {
		return
	}
	v.mu.Lock()
	v.sides[sidePlain].State, v.sides[sideAura].State = "down", "down"
	v.mu.Unlock()

	var plainBack, auraBack time.Duration
	var health map[string]any
	var up sync.WaitGroup
	up.Add(2)
	go func() {
		defer up.Done()
		if v.plain.waitHealthy(20 * time.Second) {
			plainBack = time.Since(killedAt)
		}
	}()
	go func() {
		defer up.Done()
		if h, err := v.c.waitHealthy(20 * time.Second); err == nil {
			health, auraBack = h, time.Since(killedAt)
		}
	}()
	up.Wait()
	v.mu.Lock()
	v.auraRole = "standby"
	v.mu.Unlock()
	b.plain("back_ms", ms(plainBack))
	b.aura("back_ms", ms(auraBack))
	if !b.check("the_standby_answered", auraBack > 0, "nothing answered on the node's port for 20 s") {
		return
	}
	node, _ := health["node"].(string)
	b.check("it_is_the_same_node", node == v.c.nodeID, "%s became %s", v.c.nodeID, node)
	v.cast.waitConnected(15 * time.Second)

	// Every session answering again, on both sides.
	back := [2]int{}
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		back = [2]int{}
		for side := range flows {
			for _, f := range flows[side] {
				if f.backAt.Load() > killedAt.UnixNano() {
					back[side]++
				}
			}
		}
		if back[sidePlain] == len(flows[sidePlain]) && back[sideAura] == len(flows[sideAura]) {
			break
		}
	}
	forgot := 0
	for _, f := range flows[sidePlain] {
		if f.forgot.Load() {
			forgot++
		}
	}
	b.plain("sessions", len(flows[sidePlain]))
	b.plain("sessions_back", back[sidePlain])
	b.plain("sessions_forgotten", forgot)
	b.aura("sessions", len(flows[sideAura]))
	b.aura("sessions_back", back[sideAura])
	b.check("every_session_resumed_on_the_node", back[sideAura] == len(flows[sideAura]),
		"%d of %d sessions were answering again after 30 s", back[sideAura], len(flows[sideAura]))

	// A request whose answer never came — the socket died first — is asked
	// again, with the same request id, which is how a request says "this is
	// the one I already sent". One that was answered is not.
	var wg sync.WaitGroup
	retried := [2]int{}
	for i := 0; i < n; i++ {
		if plainConn[i] != nil {
			plainConn[i].Close()
		}
		auraConn[i].close()
		if !answered[sidePlain][i].Load() {
			retried[sidePlain]++
			wg.Add(1)
			go v.retryPlain(&wg, session(sidePlain, i), order(sidePlain, i))
		}
		if !answered[sideAura][i].Load() {
			retried[sideAura]++
			wg.Add(1)
			go v.retryAura(&wg, session(sideAura, i), order(sideAura, i))
		}
	}
	wg.Wait()
	time.Sleep(500 * time.Millisecond)
	b.plain("asked_again", retried[sidePlain])
	b.aura("asked_again", retried[sideAura])
	b.check("the_crash_left_requests_unanswered", retried[sideAura] > 0,
		"every request had been answered before the kill, so nothing was asked again")
	twice, once := [2]int{}, [2]int{}
	for side := range twice {
		for i := 0; i < n; i++ {
			switch times := v.svc[side].times(order(side, i)); {
			case times >= 2:
				twice[side]++
			case times == 1:
				once[side]++
			}
		}
	}
	b.plain("carried_out_twice", twice[sidePlain])
	b.aura("carried_out_twice", twice[sideAura])
	b.aura("carried_out_once", once[sideAura])
	b.harm(sidePlain, twice[sidePlain])
	b.harm(sideAura, twice[sideAura])

	b.check("the_node_carried_out_nothing_twice", twice[sideAura] == 0,
		"%d orders were carried out twice", twice[sideAura])
	b.check("every_order_on_the_node_was_carried_out_once", once[sideAura] == n,
		"%d of %d orders were carried out exactly once", once[sideAura], n)
}

// retryPlain sends the agent a request it has been sent before.
func (v *Versus) retryPlain(wg *sync.WaitGroup, session, order string) {
	defer wg.Done()
	conn, _, err := websocket.DefaultDialer.Dial(v.plain.ws()+"/session?id="+session, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.WriteJSON(plainMsg{ID: order, Op: "work", Order: order})
	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	var r plainReply
	_ = conn.ReadJSON(&r)
}

// retryAura sends the node an envelope it has been sent before: same session,
// same id.
func (v *Versus) retryAura(wg *sync.WaitGroup, session, order string) {
	defer wg.Done()
	cl, err := v.c.open(graphWork, session)
	if err != nil {
		return
	}
	defer cl.close()
	_ = cl.sayAs(order, "op "+order)
	_, _ = cl.await(2500*time.Millisecond, func(env envelope) bool {
		return env.Kind == channel.KindData && textOf(env) == "done "+order
	})
}

// forged: one character of the record of what was done.
func (v *Versus) beatForged(b *vbeat) {
	// The agent's log, checked by the agent, before and after.
	var before, after struct {
		OK      bool `json:"ok"`
		Entries int  `json:"entries"`
	}
	v.plain.call(http.MethodGet, "/check", nil, &before)
	ed, line, err := tamperPlain(v.plain.cfg.LogPath)
	v.plain.call(http.MethodGet, "/check", nil, &after)
	b.plain("entries", after.Entries)
	if err == nil {
		b.plain("edited_line", line)
		b.plain("edited_from", ed.from)
		b.plain("edited_to", ed.to)
	}
	noticed := err != nil || !after.OK
	b.plain("noticed", noticed)
	if !noticed && before.OK {
		b.harm(sidePlain, 1)
	}

	// The node's ledger, checked by the node and then by nobody's node at all.
	entries, ok := v.c.ledgerEntries()
	if !b.check("there_is_a_ledger_to_attack", ok && entries >= 3, "the ledger holds %d entries", entries) {
		return
	}
	var sound verdict
	code, _ := v.c.call(http.MethodGet, "/v1/ledger/verify", nil, &sound)
	if !b.check("sound_before", code == 200 && sound.Sound, "the ledger did not verify before it was touched") {
		return
	}
	seq := uint64(entries / 2)
	aed, err := tamper(v.c.dir, seq)
	if !b.check("one_byte_was_edited", err == nil, "%v", err) {
		return
	}
	var broken verdict
	t := time.Now()
	code, _ = v.c.call(http.MethodGet, "/v1/ledger/verify", nil, &broken)
	b.aura("verify_ms", ms(time.Since(t)))
	b.aura("entries", sound.Report.TotalEntries)
	b.aura("edited_seq", seq)
	b.aura("edited_from", aed.from)
	b.aura("edited_to", aed.to)
	b.aura("broken_at_seq", broken.Report.BrokenAtSeq)
	offlineSound, out, _ := v.c.verifyOffline()
	caught := code == http.StatusConflict && !broken.Sound
	b.aura("noticed", caught)
	if !caught {
		b.harm(sideAura, 1)
	}

	b.check("the_node_says_not_sound", caught, "verification answered %d, sound=%v", code, broken.Sound)
	b.check("it_names_the_entry", broken.Report.BrokenAtSeq == seq || broken.Report.BrokenAtSeq == seq+1,
		"entry %d was edited and verification stopped at %d", seq, broken.Report.BrokenAtSeq)
	b.check("offline_verification_agrees", !offlineSound && strings.Contains(out, "NOT SOUND"),
		"`aura verify` did not report the ledger as unsound")
}

// tamperPlain changes one character of one line of the agent's audit log, in
// place: the same edit tamper makes to the ledger, to the same kind of field.
func tamperPlain(path string) (edit, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return edit{}, 0, err
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) < 3 {
		return edit{}, 0, fmt.Errorf("the log holds %d lines", len(lines))
	}
	n := len(lines) / 2
	offset := 0
	for _, l := range lines[:n] {
		offset += len(l) + 1
	}
	const key = `"order":"`
	at := bytes.Index(lines[n], []byte(key))
	if at < 0 {
		return edit{}, 0, errors.New("the line has no order")
	}
	start := at + len(key)
	end := start + bytes.IndexByte(lines[n][start:], '"')
	if end <= start {
		return edit{}, 0, errors.New("the line has an empty order")
	}
	from := string(lines[n][start:end])
	swap := byte('x')
	if lines[n][end-1] == swap {
		swap = 'y'
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return edit{}, 0, err
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte{swap}, int64(offset+end-1)); err != nil {
		return edit{}, 0, err
	}
	return edit{field: "order", from: from, to: from[:len(from)-1] + string(swap)}, n + 1, nil
}
