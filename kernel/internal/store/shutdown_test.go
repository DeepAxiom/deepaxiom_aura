package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The contract these tests fix: a write either lands or comes back with an
// error, never neither. It is the same contract internal/seglog is tested
// against, and it was broken here in exactly the way a `select` on a buffered
// channel plus a `stop` channel always breaks it — 56 of 100 submits after
// Close hung forever, which on this path is a session goroutine hung for the
// life of the process.

const shutdownGrace = 3 * time.Second

// submitOrHang runs one Submit and reports whether it answered at all.
func submitOrHang(t *testing.T, w *writer, i int) (err error, answered bool) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- w.Submit(
			`INSERT OR REPLACE INTO sessions(session_id, graph_id, started) VALUES(?,?,?)`,
			"shutdown-probe", "g", int64(i))
	}()
	select {
	case e := <-done:
		return e, true
	case <-time.After(shutdownGrace):
		return nil, false
	}
}

// A Submit that arrives after Close must be refused, not swallowed.
func TestSubmitAfterCloseIsRefused(t *testing.T) {
	st := open(t)
	st.w.Close()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var hung, accepted int
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err, answered := submitOrHang(t, st.w, i)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case !answered:
				hung++
			case err == nil:
				accepted++
			case !errors.Is(err, ErrClosed):
				t.Errorf("submit after Close: got %v, want ErrClosed", err)
			}
		}(i)
	}
	wg.Wait()

	if hung > 0 {
		t.Fatalf("%d/100 submits after Close neither landed nor returned an error", hung)
	}
	if accepted > 0 {
		t.Fatalf("%d/100 submits were accepted after Close", accepted)
	}
}

// A Submit racing Close must still be answered — either committed, because it
// was accepted before admission shut, or refused. The one outcome the writer
// may not produce is silence.
func TestSubmitRacingCloseAlwaysAnswers(t *testing.T) {
	st := open(t)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var hung int
	start := make(chan struct{})
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, answered := submitOrHang(t, st.w, i); !answered {
				mu.Lock()
				hung++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	st.w.Close()
	wg.Wait()

	if hung > 0 {
		t.Fatalf("%d/200 submits racing Close neither landed nor returned an error", hung)
	}
}

// Close must not return while a transaction is still open against the database,
// or Store.Close would pull the database out from under an in-flight ledger
// commit and report a clean shutdown.
func TestCloseWaitsForTheWriter(t *testing.T) {
	st := open(t)

	var wg sync.WaitGroup
	for i := 0; i < 400; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = st.w.Submit(
				`INSERT OR REPLACE INTO sessions(session_id, graph_id, started) VALUES(?,?,?)`,
				"close-wait", "g", int64(i))
		}(i)
	}
	// Close concurrently with the burst: whatever it accepted, it must have
	// committed before returning.
	st.w.Close()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		t.Fatal("submits were still outstanding after Close returned")
	}

	// The writer goroutine is gone, so the database is safe to close. If Close
	// had returned early this is where a commit would race the close.
	if err := st.db.Close(); err != nil {
		t.Fatalf("closing the database after Close: %v", err)
	}
}

// Close is idempotent: a Store closed twice (defer plus an explicit shutdown)
// must not panic on a second close of the stop channel.
func TestCloseIsIdempotent(t *testing.T) {
	st := open(t)
	st.w.Close()
	st.w.Close()
}
