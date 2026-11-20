package demo

import (
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aura/kernel/internal/channel"
)

// The load: sessions that never stop, and the instrument that times them.
//
// Two kinds of session run for the whole demo. Relay sessions send a message
// at a fixed rate and time the answer; what they measure is the node's routing
// and nothing else, because the Expertise answering them does no work. They
// are open-loop — a session sends on its clock whether or not the last answer
// is back — so a node that slows down shows up as latency rather than hiding as
// a lower send rate.
//
// Operation sessions do the whole path a decision takes: a model call through
// the node, an order, an effect sealed in the ledger, a credential released for
// it, a call to the service.

// tickEvery is the resolution of everything plotted.
const tickEvery = 250 * time.Millisecond

// recorder turns individual round-trip times into the series on the screen.
// Exact percentiles over real samples: at this volume there is no need for a
// histogram's approximation.
type recorder struct {
	mu    sync.Mutex
	cur   []float32
	ticks []tick // oldest first, bounded
}

type tick struct {
	at  time.Time // when the tick closed
	lat []float32 // sorted, milliseconds
}

const keepTicks = 480 // two minutes

func (r *recorder) add(ms float32) {
	r.mu.Lock()
	r.cur = append(r.cur, ms)
	r.mu.Unlock()
}

// roll closes the current tick.
func (r *recorder) roll(now time.Time) tick {
	r.mu.Lock()
	lat := r.cur
	r.cur = make([]float32, 0, cap(lat))
	r.mu.Unlock()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t := tick{at: now, lat: lat}
	r.mu.Lock()
	r.ticks = append(r.ticks, t)
	if len(r.ticks) > keepTicks {
		r.ticks = r.ticks[len(r.ticks)-keepTicks:]
	}
	r.mu.Unlock()
	return t
}

func percentile(sorted []float32, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)) * p)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return float64(sorted[i])
}

// window is what the recorder saw over a stretch of time.
type window struct {
	N    int     `json:"n"`
	Rate float64 `json:"rate"` // per second
	P50  float64 `json:"p50"`  // milliseconds
	P99  float64 `json:"p99"`
}

// between summarises the ticks that closed in (from, to].
func (r *recorder) between(from, to time.Time) window {
	r.mu.Lock()
	var all []float32
	for _, t := range r.ticks {
		if t.at.After(from) && !t.at.After(to) {
			all = append(all, t.lat...)
		}
	}
	r.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	w := window{N: len(all), P50: percentile(all, 0.50), P99: percentile(all, 0.99)}
	if secs := to.Sub(from).Seconds(); secs > 0 {
		w.Rate = float64(len(all)) / secs
	}
	return w
}

// ── sessions ────────────────────────────────────────────────────────

// What a dot on the screen can be.
const (
	dotOpening = iota // not yet connected
	dotFlowing        // a relay session, answering
	dotOps            // an operation session, answering
	dotSlow           // its reader has stopped
	dotDown           // lost its node; reconnecting
	dotForgot         // answering again, by something that no longer knows it
)

const dotChars = ".fosxa"

// flow is one session that runs for the whole demo.
type flow struct {
	id    string
	graph string
	state atomic.Int32

	sent      atomic.Int64
	got       atomic.Int64
	lastReply atomic.Int64 // unix nanos
	// backAt is when this session next answered after going down.
	backAt atomic.Int64

	// turn is how far along the other end last said this session was, and
	// forgot is set once it has said something lower. Only the comparison's
	// plain agent reports a turn.
	turn   atomic.Int64
	forgot atomic.Bool

	// lent is set while a beat has taken this session over; the session's own
	// loop steps aside and its socket is closed.
	lent atomic.Bool

	mu    sync.Mutex
	conn  *client
	plain io.Closer // the socket, when the session is with the plain agent
}

func (f *flow) setConn(cl *client) {
	f.mu.Lock()
	f.conn = cl
	f.mu.Unlock()
}

func (f *flow) setPlain(c io.Closer) {
	f.mu.Lock()
	f.plain = c
	f.mu.Unlock()
}

// kick closes the session's socket, if it has one.
func (f *flow) kick() {
	f.mu.Lock()
	if f.conn != nil {
		f.conn.close()
	}
	if f.plain != nil {
		_ = f.plain.Close()
	}
	f.mu.Unlock()
}

// lanes is every long-running session.
type lanes struct {
	c    *cluster
	rate float64 // messages per second, per relay session

	relay []*flow
	ops   []*flow

	rec    recorder // relay round trips
	opsRec recorder // whole operations: message to effect confirmed

	interrupted atomic.Int64 // relay messages unanswered when their socket died
	opsDone     atomic.Int64
	opsFailed   atomic.Int64
	tokensSeen  atomic.Int64 // tokens operation clients received

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

func (c *cluster) lanes(relay, ops int, rate float64) *lanes {
	l := &lanes{c: c, rate: rate, stop: make(chan struct{})}
	stamp := time.Now().UnixNano() % 1_000_000
	for i := 0; i < relay; i++ {
		l.relay = append(l.relay, &flow{graph: graphRelay, id: fmt.Sprintf("demo-r-%d-%03d", stamp, i)})
	}
	for i := 0; i < ops; i++ {
		l.ops = append(l.ops, &flow{graph: graphOps, id: fmt.Sprintf("demo-o-%d-%03d", stamp, i)})
	}
	return l
}

func (l *lanes) stopped() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

// start opens every session, staggered over ramp so the picture is of a node
// taking on load rather than of a thundering herd.
func (l *lanes) start(ramp time.Duration) {
	// Operation sessions are interleaved with the relay ones rather than all
	// opened last.
	type job struct {
		f   *flow
		ops bool
	}
	var jobs []job
	every, o := 0, 0
	if len(l.ops) > 0 {
		every = max(1, len(l.relay)/len(l.ops))
	}
	for i, f := range l.relay {
		jobs = append(jobs, job{f, false})
		if every > 0 && i%every == 0 && o < len(l.ops) {
			jobs = append(jobs, job{l.ops[o], true})
			o++
		}
	}
	for ; o < len(l.ops); o++ {
		jobs = append(jobs, job{l.ops[o], true})
	}
	if len(jobs) == 0 {
		return
	}
	gap := ramp / time.Duration(len(jobs))
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		for _, j := range jobs {
			if l.stopped() {
				return
			}
			l.wg.Add(1)
			if j.ops {
				go l.runOps(j.f)
			} else {
				go l.runRelay(j.f)
			}
			time.Sleep(gap)
		}
	}()
}

func (l *lanes) close() {
	l.stopOnce.Do(func() { close(l.stop) })
	for _, f := range append(append([]*flow{}, l.relay...), l.ops...) {
		f.kick()
	}
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// dots renders every session as one character.
func (l *lanes) dots() string {
	var b strings.Builder
	b.Grow(len(l.relay) + len(l.ops))
	for _, f := range l.relay {
		b.WriteByte(dotChars[f.state.Load()])
	}
	for _, f := range l.ops {
		b.WriteByte(dotChars[f.state.Load()])
	}
	return b.String()
}

// flowing counts sessions answering right now.
func (l *lanes) flowing() (relay, ops int) {
	for _, f := range l.relay {
		if f.state.Load() == dotFlowing {
			relay++
		}
	}
	for _, f := range l.ops {
		if f.state.Load() == dotOps {
			ops++
		}
	}
	return
}

// sendRing is how many relay messages a session can have unanswered and still
// time correctly. At ten a second that is twenty-five seconds of silence.
const sendRing = 256

// waitWhileLent parks a session's loop while a beat has it. It reports false
// when the demo is stopping.
func (l *lanes) waitWhileLent(f *flow) bool {
	for f.lent.Load() {
		if l.stopped() {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return !l.stopped()
}

func (l *lanes) runRelay(f *flow) {
	defer l.wg.Done()
	interval := time.Duration(float64(time.Second) / l.rate)
	sentAt := make([]atomic.Int64, sendRing)
	var n int64 // message number, continuous across reconnects
	wasDown := false

	for l.waitWhileLent(f) {
		cl, err := l.c.open(f.graph, f.id)
		if err != nil {
			if f.state.Load() != dotOpening {
				f.state.Store(dotDown)
			}
			time.Sleep(time.Duration(8+rand.Intn(24)) * time.Millisecond)
			continue
		}
		f.setConn(cl)
		if f.lent.Load() { // lent while dialling
			cl.close()
			continue
		}
		f.state.Store(dotFlowing)

		done := make(chan struct{})
		go func() {
			// A random phase, so three hundred sessions do not all send on
			// the same millisecond.
			select {
			case <-done:
				return
			case <-time.After(time.Duration(rand.Int63n(int64(interval)))):
			}
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-l.stop:
					return
				case <-t.C:
					k := atomic.AddInt64(&n, 1)
					sentAt[k%sendRing].Store(time.Now().UnixNano())
					if _, err := cl.say(strconv.FormatInt(k, 10)); err != nil {
						return
					}
					f.sent.Add(1)
				}
			}
		}()

		for {
			var env envelope
			_ = cl.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			if err := cl.conn.ReadJSON(&env); err != nil {
				break
			}
			if env.Kind == channel.KindError {
				// The node could not deliver for this session — the Expertise
				// it was bound to is gone, say. Resuming the session binds it
				// again, which is what a client does.
				break
			}
			if env.Kind != channel.KindData {
				continue
			}
			k, err := strconv.ParseInt(textOf(env), 10, 64)
			if err != nil {
				continue
			}
			now := time.Now().UnixNano()
			if at := sentAt[k%sendRing].Swap(0); at != 0 {
				l.rec.add(float32(float64(now-at) / 1e6))
			}
			f.got.Add(1)
			f.lastReply.Store(now)
			if wasDown {
				wasDown = false
				f.backAt.Store(now)
			}
		}
		close(done)
		cl.close()
		f.setConn(nil)
		if l.stopped() {
			return
		}
		if !f.lent.Load() {
			f.state.Store(dotDown)
			wasDown = true
			// Whatever was unanswered when the socket died stays unanswered:
			// the reply was addressed to a connection that no longer exists.
			for i := range sentAt {
				if sentAt[i].Swap(0) != 0 {
					l.interrupted.Add(1)
				}
			}
		}
	}
}

func (l *lanes) runOps(f *flow) {
	defer l.wg.Done()
	var n int64
	wasDown := false
	for l.waitWhileLent(f) {
		cl, err := l.c.open(f.graph, f.id)
		if err != nil {
			if f.state.Load() != dotOpening {
				f.state.Store(dotDown)
			}
			time.Sleep(time.Duration(8+rand.Intn(24)) * time.Millisecond)
			continue
		}
		f.setConn(cl)
		f.state.Store(dotOps)
		for !l.stopped() && !f.lent.Load() {
			n++
			order := fmt.Sprintf("%s-%d", f.id, n)
			start := time.Now()
			if _, err := cl.say("op " + order); err != nil {
				break
			}
			f.sent.Add(1)
			refused := false
			_, err := cl.await(8*time.Second, func(env envelope) bool {
				if env.Kind == channel.KindError {
					refused = true
					return true
				}
				if env.Kind != channel.KindData {
					return false
				}
				text := textOf(env)
				if !strings.HasPrefix(text, "done ") {
					l.tokensSeen.Add(1)
					return false
				}
				return text == "done "+order
			})
			if err != nil || refused {
				l.opsFailed.Add(1)
				break
			}
			now := time.Now()
			l.opsRec.add(float32(now.Sub(start).Seconds() * 1000))
			l.opsDone.Add(1)
			f.got.Add(1)
			f.lastReply.Store(now.UnixNano())
			if wasDown {
				wasDown = false
				f.backAt.Store(now.UnixNano())
			}
			select {
			case <-l.stop:
			case <-time.After(time.Duration(600+rand.Intn(500)) * time.Millisecond):
			}
		}
		cl.close()
		f.setConn(nil)
		if l.stopped() {
			return
		}
		if !f.lent.Load() {
			f.state.Store(dotDown)
			wasDown = true
		}
	}
}
