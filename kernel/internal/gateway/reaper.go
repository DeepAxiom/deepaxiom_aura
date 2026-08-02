package gateway

import (
	"sync"
	"time"
)

// Bounded bookkeeping for the inbound ingress path.
//
// Everything here exists because /hooks/{name} is the one surface whose call
// rate the node does not control. A client socket is opened by someone the
// node authenticated; a webhook is posted by whoever found the URL. So the
// structures on that path have to be bounded by design rather than by the
// sender's good behaviour.

// sessionReaper ends ingress sessions after their TTL.
//
// It replaces a goroutine-per-delivery that slept for two minutes: at a
// thousand deliveries a minute that was two thousand parked goroutines, each
// holding a session, and nothing in the shape of the code said so. One
// goroutine walking a deadline-ordered queue has the same behaviour and a flat
// cost.
type sessionReaper struct {
	mu      sync.Mutex
	pending []reapEntry
	wake    chan struct{}
	end     func(sessionID string)
	ttl     time.Duration
	started bool
}

type reapEntry struct {
	sessionID string
	due       time.Time
}

func newSessionReaper(ttl time.Duration, end func(string)) *sessionReaper {
	return &sessionReaper{
		wake: make(chan struct{}, 1),
		end:  end,
		ttl:  ttl,
	}
}

// add schedules a session to be ended once its TTL elapses. Entries are
// appended in due order because every entry gets the same TTL, so the queue is
// sorted by construction and the reaper only ever inspects its head.
func (s *sessionReaper) add(sessionID string) {
	s.mu.Lock()
	s.pending = append(s.pending, reapEntry{sessionID: sessionID, due: time.Now().Add(s.ttl)})
	if !s.started {
		s.started = true
		go s.run()
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *sessionReaper) run() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.mu.Lock()
		var wait time.Duration
		now := time.Now()
		for len(s.pending) > 0 && !s.pending[0].due.After(now) {
			entry := s.pending[0]
			s.pending = s.pending[1:]
			s.mu.Unlock()
			s.end(entry.sessionID)
			s.mu.Lock()
			now = time.Now()
		}
		if len(s.pending) > 0 {
			wait = time.Until(s.pending[0].due)
		} else {
			wait = time.Hour // nothing queued; the wake channel does the work
		}
		s.mu.Unlock()

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-timer.C:
		case <-s.wake:
		}
	}
}

// reapSession schedules an ingress session for cleanup.
func (g *Gateway) reapSession(sessionID string) {
	g.reaperOnce.Do(func() {
		g.reaper = newSessionReaper(ingressSessionTTL, g.Mgr.End)
	})
	g.reaper.add(sessionID)
}

// routeRate is a fixed-window counter per ingress route.
//
// Fixed rather than sliding on purpose: this is a ceiling that stops a public
// endpoint being turned into a session generator, not a meter anybody bills
// from, and a sliding window costs memory proportional to the traffic it is
// meant to be protecting against.
type routeRate struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

// hookRate reports whether a delivery on this route is within its limit.
func (g *Gateway) hookRate(route string, perMinute int) bool {
	if perMinute <= 0 {
		return true
	}
	g.rateOnce.Do(func() {
		g.rate = &routeRate{windows: map[string]*rateWindow{}}
	})
	r := g.rate

	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.windows[route]
	if !ok {
		// Routes are declared, not arbitrary, so this map is bounded by
		// configuration. The cap is a backstop against a bug, not a workload.
		if len(r.windows) > 4096 {
			r.windows = map[string]*rateWindow{}
		}
		w = &rateWindow{start: now}
		r.windows[route] = w
	}
	if now.Sub(w.start) >= time.Minute {
		w.start = now
		w.count = 0
	}
	if w.count >= perMinute {
		return false
	}
	w.count++
	return true
}
