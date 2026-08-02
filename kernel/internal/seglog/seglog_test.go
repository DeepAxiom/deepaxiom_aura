package seglog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// This package holds a node's replayable history, so the tests that matter are
// the ones about losing it: a crash mid-write, a truncated tail, a file someone
// edited. Throughput is the reason it exists; correctness is the reason it is
// allowed to.

func open(t *testing.T, dir string) *Log {
	t.Helper()
	l, err := Open(dir, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func entry(session, id, kind, text string) Entry {
	return Entry{
		Session: session, MsgID: id, CauseID: "c-" + id, Kind: kind, TS: 1700000000000,
		Envelope: []byte(`{"kind":"` + kind + `","payload":{"text":"` + text + `"}}`),
	}
}

func TestAppendAndReadBackInOrder(t *testing.T) {
	l := open(t, t.TempDir())
	for i := 0; i < 100; i++ {
		if err := l.Append(entry("s1", fmt.Sprintf("m%d", i), "data", fmt.Sprintf("t%d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := l.Session("s1", 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("read back %d records, want 100", len(got))
	}
	for i, e := range got {
		if e.MsgID != fmt.Sprintf("m%d", i) {
			t.Fatalf("position %d holds %s — order was not preserved", i, e.MsgID)
		}
		if e.Kind != "data" || e.CauseID != "c-"+e.MsgID || e.TS != 1700000000000 {
			t.Fatalf("field lost on the round trip: %+v", e)
		}
		want := fmt.Sprintf(`{"kind":"data","payload":{"text":"t%d"}}`, i)
		if string(e.Envelope) != want {
			t.Fatalf("envelope %d = %s", i, e.Envelope)
		}
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	l := open(t, t.TempDir())
	// Interleaved on disk, which is the normal case and the one a naive index
	// gets wrong.
	for i := 0; i < 30; i++ {
		_ = l.Append(entry(fmt.Sprintf("s%d", i%3), fmt.Sprintf("m%d", i), "data", "x"))
	}
	for s := 0; s < 3; s++ {
		got, err := l.Session(fmt.Sprintf("s%d", s), 0)
		if err != nil {
			t.Fatalf("session %d: %v", s, err)
		}
		if len(got) != 10 {
			t.Fatalf("session s%d has %d records, want 10", s, len(got))
		}
		for _, e := range got {
			if e.Session != fmt.Sprintf("s%d", s) {
				t.Fatalf("session s%d leaked a record from %s", s, e.Session)
			}
		}
	}
	if _, err := l.Session("nope", 0); err != nil {
		t.Errorf("an unknown session should read empty, not fail: %v", err)
	}
}

func TestLimitTakesTheOldest(t *testing.T) {
	l := open(t, t.TempDir())
	for i := 0; i < 50; i++ {
		_ = l.Append(entry("s1", fmt.Sprintf("m%d", i), "data", "x"))
	}
	got, _ := l.Session("s1", 10)
	if len(got) != 10 || got[0].MsgID != "m0" || got[9].MsgID != "m9" {
		t.Fatalf("limit did not take the first ten: %d records, first %s", len(got), got[0].MsgID)
	}
}

func TestCountsFeedTheSessionListing(t *testing.T) {
	l := open(t, t.TempDir())
	for i := 0; i < 7; i++ {
		_ = l.Append(entry("s1", fmt.Sprintf("m%d", i), "data", "x"))
	}
	for i := 0; i < 3; i++ {
		_ = l.Append(entry("s1", fmt.Sprintf("e%d", i), "error", "boom"))
	}
	total, errs := l.Counts("s1")
	if total != 10 || errs != 3 {
		t.Fatalf("counts = (%d, %d), want (10, 3)", total, errs)
	}
	if total, errs := l.Counts("missing"); total != 0 || errs != 0 {
		t.Errorf("an unknown session should count zero, got (%d, %d)", total, errs)
	}
}

// Reopening must reconstruct the index from the file alone — that is what makes
// the log the source of truth rather than the in-memory map beside it.
func TestIndexIsRebuiltOnReopen(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 40; i++ {
		kind := "data"
		if i%10 == 0 {
			kind = "error"
		}
		_ = l.Append(entry("s1", fmt.Sprintf("m%d", i), kind, "x"))
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	l2 := open(t, dir)
	got, err := l2.Session("s1", 0)
	if err != nil {
		t.Fatalf("session after reopen: %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("reopen recovered %d of 40 records", len(got))
	}
	if got[0].MsgID != "m0" || got[39].MsgID != "m39" {
		t.Error("order was not preserved across a reopen")
	}
	if total, errs := l2.Counts("s1"); total != 40 || errs != 4 {
		t.Errorf("counts after reopen = (%d, %d), want (40, 4)", total, errs)
	}
	// And it must keep accepting writes at the right offset.
	if err := l2.Append(entry("s1", "m40", "data", "x")); err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if got, _ := l2.Session("s1", 0); len(got) != 41 {
		t.Errorf("append after reopen produced %d records", len(got))
	}
}

// A power cut lands mid-record. Everything before it must survive, the torn
// record must not be read as data, and the log must be writable again.
func TestTornTailIsTruncatedNotRead(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, false)
	for i := 0; i < 20; i++ {
		_ = l.Append(entry("s1", fmt.Sprintf("m%d", i), "data", "x"))
	}
	_ = l.Close()

	path := filepath.Join(dir, "events.log")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Append half a record: a plausible header promising bytes that never came.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	_, _ = f.Write([]byte{0, 0, 1, 0, 0xDE, 0xAD, 0xBE, 0xEF, 'p', 'a', 'r', 't'})
	_ = f.Close()

	l2 := open(t, dir)
	got, err := l2.Session("s1", 0)
	if err != nil {
		t.Fatalf("session after torn tail: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("a torn tail cost %d intact records", 20-len(got))
	}
	if after, _ := os.Stat(path); after.Size() != info.Size() {
		t.Errorf("file is %d bytes, want the %d that were intact — the tail was not truncated",
			after.Size(), info.Size())
	}
	if err := l2.Append(entry("s1", "m20", "data", "x")); err != nil {
		t.Fatalf("the log must be writable after repair: %v", err)
	}
	if got, _ := l2.Session("s1", 0); len(got) != 21 {
		t.Errorf("write after repair produced %d records", len(got))
	}
}

// A record altered in place must be refused, not returned as if it were real.
// The log is replay evidence; silently serving edited history is the one
// failure that would make it worthless.
func TestAlteredRecordIsRefused(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, false)
	for i := 0; i < 5; i++ {
		_ = l.Append(entry("s1", fmt.Sprintf("m%d", i), "data", "original"))
	}
	_ = l.Close()

	// Flip a byte inside the payload of a record the index already knows about.
	path := filepath.Join(dir, "events.log")
	raw, _ := os.ReadFile(path)
	idx := -1
	for i := 0; i+8 < len(raw); i++ {
		if string(raw[i:i+8]) == "original" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("test fixture: payload not found in the file")
	}
	raw[idx] = 'X'
	_ = os.WriteFile(path, raw, 0o644)

	l2 := open(t, dir)
	// Recovery stops at the bad record, so the ones before it survive and the
	// altered one is not in the index.
	got, _ := l2.Session("s1", 0)
	for _, e := range got {
		if string(e.Envelope) == `{"kind":"data","payload":{"text":"Xriginal"}}` {
			t.Fatal("an altered record was served as genuine")
		}
	}
	if len(got) >= 5 {
		t.Errorf("recovery accepted %d records; it should have stopped at the altered one", len(got))
	}
}

// Concurrent writers must not interleave records or lose any.
func TestConcurrentAppendsAreAllDurableAndWellFormed(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, false)
	const writers, per = 40, 100

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := l.Append(entry(fmt.Sprintf("s%d", w),
					fmt.Sprintf("m%d", i), "data", "x")); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	_ = l.Close()

	// Reopened, so this checks what reached disk rather than what memory says.
	l2 := open(t, dir)
	for w := 0; w < writers; w++ {
		got, err := l2.Session(fmt.Sprintf("s%d", w), 0)
		if err != nil {
			t.Fatalf("session s%d: %v", w, err)
		}
		if len(got) != per {
			t.Fatalf("session s%d has %d of %d records after a reopen", w, len(got), per)
		}
		for i, e := range got {
			if e.MsgID != fmt.Sprintf("m%d", i) {
				t.Fatalf("session s%d lost per-session order at %d: %s", w, i, e.MsgID)
			}
		}
	}
}

func TestEmptyAndLargeEnvelopes(t *testing.T) {
	l := open(t, t.TempDir())
	big := make([]byte, 2<<20) // a realtime audio frame, before elision
	for i := range big {
		big[i] = 'a'
	}
	cases := []Entry{
		{Session: "s1", MsgID: "empty", Kind: "done", TS: 1},
		{Session: "s1", MsgID: "big", Kind: "data", TS: 2, Envelope: big},
		{Session: "s1", MsgID: "nocause", Kind: "data", TS: 3, Envelope: []byte(`{}`)},
	}
	for _, c := range cases {
		if err := l.Append(c); err != nil {
			t.Fatalf("append %s: %v", c.MsgID, err)
		}
	}
	got, err := l.Session("s1", 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("read back %d records: %v", len(got), err)
	}
	if len(got[0].Envelope) != 0 || len(got[1].Envelope) != len(big) || string(got[2].Envelope) != `{}` {
		t.Errorf("envelope sizes did not survive: %d, %d, %q",
			len(got[0].Envelope), len(got[1].Envelope), got[2].Envelope)
	}
}

func TestOversizedRecordIsRefusedRatherThanWritten(t *testing.T) {
	l := open(t, t.TempDir())
	err := l.Append(Entry{Session: "s1", MsgID: "huge", Kind: "data",
		Envelope: make([]byte, maxRecord+1)})
	if err == nil {
		t.Fatal("a record past the cap must be refused, not written")
	}
	if got, _ := l.Session("s1", 0); len(got) != 0 {
		t.Errorf("the refused record left %d entries behind", len(got))
	}
}

func TestAppendAfterCloseIsRefused(t *testing.T) {
	l, _ := Open(t.TempDir(), false)
	_ = l.Append(entry("s1", "m0", "data", "x"))
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := l.Append(entry("s1", "m1", "data", "x")); err == nil {
		t.Error("appending to a closed log must fail rather than hang or pretend")
	}
}

func TestSyncModeStillRoundTrips(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, true) // fsync every batch
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 20; i++ {
		if err := l.Append(entry("s1", fmt.Sprintf("m%d", i), "data", "x")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	l2 := open(t, dir)
	if got, _ := l2.Session("s1", 0); len(got) != 20 {
		t.Errorf("fsync mode recovered %d of 20", len(got))
	}
}
