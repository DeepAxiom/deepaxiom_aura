package seglog

import (
	"fmt"
	"testing"
)

// walk reads a whole session through SessionPage and returns the ids in order.
func walk(t *testing.T, l *Log, session string, page int) []string {
	t.Helper()
	var ids []string
	for offset := 0; ; offset += page {
		got, more, err := l.SessionPage(session, offset, page)
		if err != nil {
			t.Fatalf("SessionPage(%d, %d): %v", offset, page, err)
		}
		for _, e := range got {
			ids = append(ids, e.MsgID)
		}
		if !more {
			return ids
		}
		if len(got) == 0 {
			t.Fatalf("SessionPage reported more records and returned none at offset %d", offset)
		}
	}
}

func TestSessionPageWalksAWholeSessionInOrder(t *testing.T) {
	// Small segments, so the walk crosses many of them.
	l := openWith(t, t.TempDir(), Options{SegmentBytes: 1 << 10, MaxIndexed: -1})
	const n = 517 // deliberately not a multiple of any page size below
	for i := 0; i < n; i++ {
		if err := l.Append(entry("s1", fmt.Sprintf("m%04d", i), "data", "x")); err != nil {
			t.Fatalf("append: %v", err)
		}
		// Another session interleaved, to be skipped rather than returned.
		if err := l.Append(entry("s2", fmt.Sprintf("o%04d", i), "data", "y")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	for _, page := range []int{1, 7, 100, 517, 10_000} {
		ids := walk(t, l, "s1", page)
		if len(ids) != n {
			t.Fatalf("page size %d: walked %d records, want %d", page, len(ids), n)
		}
		for i, id := range ids {
			if want := fmt.Sprintf("m%04d", i); id != want {
				t.Fatalf("page size %d: record %d is %s, want %s", page, i, id, want)
			}
		}
	}
}

func TestSessionPageSaysWhetherThereIsMore(t *testing.T) {
	l := open(t, t.TempDir())
	for i := 0; i < 10; i++ {
		if err := l.Append(entry("s1", fmt.Sprintf("m%d", i), "data", "x")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	for _, tc := range []struct {
		offset, limit, want int
		more                bool
	}{
		{0, 4, 4, true},
		{4, 4, 4, true},
		{8, 4, 2, false},
		{0, 10, 10, false}, // exactly the end is not "more"
		{10, 4, 0, false},  // past the end
		{0, 0, 0, false},   // nothing asked for
	} {
		got, more, err := l.SessionPage("s1", tc.offset, tc.limit)
		if err != nil {
			t.Fatalf("SessionPage(%d,%d): %v", tc.offset, tc.limit, err)
		}
		if len(got) != tc.want || more != tc.more {
			t.Fatalf("SessionPage(%d,%d) = %d records, more=%v; want %d, more=%v",
				tc.offset, tc.limit, len(got), more, tc.want, tc.more)
		}
	}
	if got, more, err := l.SessionPage("nobody", 0, 10); err != nil || len(got) != 0 || more {
		t.Fatalf("an unknown session returned %d records, more=%v, err=%v", len(got), more, err)
	}
}

// A session whose locators were evicted from the index cannot be addressed by
// offset, so the page comes from a scan. It has to be the same page.
func TestSessionPageReadsAnEvictedSession(t *testing.T) {
	// One indexed record allowed: every session but the hottest is evicted.
	l := openWith(t, t.TempDir(), Options{SegmentBytes: 1 << 10, MaxIndexed: 1})
	const n = 60
	for i := 0; i < n; i++ {
		if err := l.Append(entry("cold", fmt.Sprintf("m%03d", i), "data", "x")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := l.Append(entry("hot", fmt.Sprintf("h%d", i), "data", "x")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	ids := walk(t, l, "cold", 16)
	if len(ids) != n {
		t.Fatalf("walked %d records of an evicted session, want %d", len(ids), n)
	}
	for i, id := range ids {
		if want := fmt.Sprintf("m%03d", i); id != want {
			t.Fatalf("record %d is %s, want %s", i, id, want)
		}
	}
}
