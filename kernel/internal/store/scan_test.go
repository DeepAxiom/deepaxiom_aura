package store

import (
	"fmt"
	"testing"
)

// SessionEvents answers "all" with the first thousand. ScanSession is for the
// callers that meant all.
func TestScanSessionWalksEveryEventInOrder(t *testing.T) {
	st := open(t)
	const n = 3*scanPage + 17 // several pages and a partial one
	for i := 0; i < n; i++ {
		body := fmt.Sprintf(`{"kind":"data","n":%d}`, i)
		if err := st.AppendEvent("s1", fmt.Sprintf("m%d", i), "", "data", []byte(body)); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	if err := st.AppendEvent("other", "x", "", "data", []byte(`{"kind":"data"}`)); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	capped, _, err := st.SessionEvents("s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) >= n {
		t.Fatalf("precondition: SessionEvents(_, 0) returned all %d events; this test "+
			"exists because it does not", len(capped))
	}

	seen := 0
	err = st.ScanSession("s1", func(envelope []byte, ts int64) bool {
		if want := fmt.Sprintf(`{"kind":"data","n":%d}`, seen); string(envelope) != want {
			t.Fatalf("event %d is %s, want %s", seen, envelope, want)
		}
		if ts == 0 {
			t.Fatalf("event %d has no timestamp", seen)
		}
		seen++
		return true
	})
	if err != nil {
		t.Fatalf("ScanSession: %v", err)
	}
	if seen != n {
		t.Fatalf("walked %d events, want %d", seen, n)
	}
}

func TestScanSessionStopsWhenTold(t *testing.T) {
	st := open(t)
	for i := 0; i < 2*scanPage; i++ {
		if err := st.AppendEvent("s1", fmt.Sprintf("m%d", i), "", "data", []byte(`{}`)); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	seen := 0
	if err := st.ScanSession("s1", func([]byte, int64) bool {
		seen++
		return seen < 5
	}); err != nil {
		t.Fatalf("ScanSession: %v", err)
	}
	if seen != 5 {
		t.Fatalf("walk continued for %d events after being told to stop at 5", seen)
	}
}

func TestScanSessionOfAnUnknownSessionIsEmpty(t *testing.T) {
	st := open(t)
	called := false
	if err := st.ScanSession("nobody", func([]byte, int64) bool { called = true; return true }); err != nil {
		t.Fatalf("ScanSession: %v", err)
	}
	if called {
		t.Fatal("a session with no history produced an event")
	}
}
