package seglog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Segmentation exists to bound three things a single file cannot bound: the
// size of one file, the bytes on disk, and the memory the index holds. These
// tests are about those bounds, and about the one thing that must survive them
// — that a read still returns the same history in the same order.

func openWith(t *testing.T, dir string, opt Options) *Log {
	t.Helper()
	l, err := OpenWith(dir, opt)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// fill appends n records to one session and returns the log.
func fill(t *testing.T, l *Log, session string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Append(entry(session, fmt.Sprintf("m%d", i), "data", "payload padding")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

func TestRotationStartsANewSegment(t *testing.T) {
	dir := t.TempDir()
	l := openWith(t, dir, Options{SegmentBytes: 2 << 10, MaxIndexed: -1})
	fill(t, l, "s1", 300)

	if got := l.Stats().Segments; got < 2 {
		t.Fatalf("300 records into 2 KiB segments produced %d segment(s); rotation never happened", got)
	}
	// The history is one history regardless of how many files it spans.
	got, err := l.Session("s1", 0)
	if err != nil {
		t.Fatalf("session across segments: %v", err)
	}
	if len(got) != 300 {
		t.Fatalf("read back %d of 300 records across segments", len(got))
	}
	for i, e := range got {
		if e.MsgID != fmt.Sprintf("m%d", i) {
			t.Fatalf("record %d is %s — order was not preserved across a rotation", i, e.MsgID)
		}
	}
}

// A record is never split across two segments, whatever the threshold says.
// Recovery frames records by length prefix, so a split one would be
// unrecoverable garbage at the head of the next file.
func TestARecordIsNeverSplitAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	l := openWith(t, dir, Options{SegmentBytes: 64, MaxIndexed: -1})
	fill(t, l, "s1", 50)
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Every segment must parse cleanly from its first byte on reopen; a split
	// record would show up as damage or as lost records.
	l2 := openWith(t, dir, Options{SegmentBytes: 64, MaxIndexed: -1})
	st := l2.Stats()
	if len(st.Damage) != 0 {
		t.Fatalf("segments did not reparse cleanly: %v", st.Damage)
	}
	if got, _ := l2.Session("s1", 0); len(got) != 50 {
		t.Fatalf("reopen across %d segments recovered %d of 50", st.Segments, len(got))
	}
}

func TestRotationSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	l := openWith(t, dir, Options{SegmentBytes: 2 << 10, MaxIndexed: -1})
	fill(t, l, "s1", 200)
	segments := l.Stats().Segments
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	l2 := openWith(t, dir, Options{SegmentBytes: 2 << 10, MaxIndexed: -1})
	if got := l2.Stats().Segments; got != segments {
		t.Errorf("reopen found %d segments, want %d", got, segments)
	}
	got, _ := l2.Session("s1", 0)
	if len(got) != 200 {
		t.Fatalf("reopen recovered %d of 200 records", len(got))
	}
	// And it must keep writing into the segment it left off in.
	if err := l2.Append(entry("s1", "m200", "data", "x")); err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if got, _ := l2.Session("s1", 0); len(got) != 201 {
		t.Errorf("append after reopen produced %d records", len(got))
	}
}

// MaxBytes reclaims space by deleting whole segments, oldest first. The active
// segment is never deleted, because a node must always have somewhere to write.
func TestRetentionDropsOldestSegmentsWhole(t *testing.T) {
	dir := t.TempDir()
	l := openWith(t, dir, Options{SegmentBytes: 2 << 10, MaxBytes: 8 << 10, MaxIndexed: -1})
	fill(t, l, "s1", 2000)

	st := l.Stats()
	if st.Bytes > 8<<10+(2<<10) {
		t.Errorf("log is %d bytes against a %d budget — retention did not reclaim", st.Bytes, 8<<10)
	}
	if st.Segments < 1 {
		t.Fatal("retention deleted every segment; the active one must survive")
	}
	// What remains must still be readable and still be in order.
	got, err := l.Session("s1", 0)
	if err != nil {
		t.Fatalf("session after retention: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("retention left nothing readable")
	}
	if len(got) >= 2000 {
		t.Errorf("retention kept all %d records; nothing was reclaimed", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].TS > got[i].TS {
			t.Fatal("order was not preserved through retention")
		}
	}
	// Counts must describe what is readable, not what was once written.
	if total, _ := l.Counts("s1"); total != len(got) {
		t.Errorf("Counts says %d but a read returns %d — the listing is claiming history the node cannot produce",
			total, len(got))
	}
}

func TestRetentionOffByDefaultKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	l := openWith(t, dir, Options{SegmentBytes: 1 << 10, MaxIndexed: -1})
	fill(t, l, "s1", 500)
	if got, _ := l.Session("s1", 0); len(got) != 500 {
		t.Fatalf("default retention lost %d records; it must keep everything", 500-len(got))
	}
}

// The index is derived state, so it is capped — and a session whose locators
// were evicted must still read back exactly, by scanning the segments the index
// still remembers it in.
func TestEvictedSessionStillReadsBackExactly(t *testing.T) {
	dir := t.TempDir()
	// A cap far below the record count forces eviction almost immediately.
	l := openWith(t, dir, Options{SegmentBytes: 4 << 10, MaxIndexed: 32})

	const sessions, each = 12, 60
	for s := 0; s < sessions; s++ {
		fill(t, l, fmt.Sprintf("s%d", s), each)
	}

	st := l.Stats()
	if st.Evicted == 0 {
		t.Fatal("nothing was evicted; the index cap did not bind")
	}
	if st.Indexed > 32 {
		t.Errorf("index holds %d locators against a cap of 32", st.Indexed)
	}

	// Every session — evicted or not — must read back its own records, in
	// order, and no one else's.
	for s := 0; s < sessions; s++ {
		id := fmt.Sprintf("s%d", s)
		got, err := l.Session(id, 0)
		if err != nil {
			t.Fatalf("session %s: %v", id, err)
		}
		if len(got) != each {
			t.Fatalf("session %s read back %d of %d records", id, len(got), each)
		}
		for i, e := range got {
			if e.Session != id {
				t.Fatalf("session %s got a record belonging to %s", id, e.Session)
			}
			if e.MsgID != fmt.Sprintf("m%d", i) {
				t.Fatalf("session %s record %d is %s — order was not preserved", id, i, e.MsgID)
			}
		}
	}
}

func TestEvictedSessionHonoursLimit(t *testing.T) {
	dir := t.TempDir()
	l := openWith(t, dir, Options{SegmentBytes: 4 << 10, MaxIndexed: 16})
	for s := 0; s < 8; s++ {
		fill(t, l, fmt.Sprintf("s%d", s), 40)
	}
	got, err := l.Session("s0", 5)
	if err != nil {
		t.Fatalf("limited read of an evicted session: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("limit 5 returned %d records", len(got))
	}
	if got[0].MsgID != "m0" {
		t.Errorf("limit took %s first; it must take the oldest", got[0].MsgID)
	}
}

// A data directory written before this package was segmented must keep its
// history. Silently starting a fresh log next to it would lose every session an
// operator could previously replay.
func TestLegacyEventsLogIsMigratedNotAbandoned(t *testing.T) {
	dir := t.TempDir()

	// Produce a real log, then rename it back to the pre-segmentation name to
	// stand in for an older node's data directory.
	l := openWith(t, dir, Options{MaxIndexed: -1})
	fill(t, l, "s1", 25)
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	segments, err := filepath.Glob(filepath.Join(dir, segPrefix+"*"+segSuffix))
	if err != nil || len(segments) != 1 {
		t.Fatalf("fixture: expected one segment, got %v (%v)", segments, err)
	}
	legacy := filepath.Join(dir, legacyName)
	if err := os.Rename(segments[0], legacy); err != nil {
		t.Fatalf("fixture rename: %v", err)
	}

	l2 := openWith(t, dir, Options{MaxIndexed: -1})
	got, err := l2.Session("s1", 0)
	if err != nil {
		t.Fatalf("session after migration: %v", err)
	}
	if len(got) != 25 {
		t.Fatalf("migration recovered %d of 25 records", len(got))
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("the legacy file is still there; it should have been renamed, not copied")
	}
	if err := l2.Append(entry("s1", "m25", "data", "x")); err != nil {
		t.Fatalf("append after migration: %v", err)
	}
	if got, _ := l2.Session("s1", 0); len(got) != 26 {
		t.Errorf("append after migration produced %d records", len(got))
	}
}

// Framing is per segment, so a segment that was altered after it was sealed
// must cost its own tail and nothing else. That containment is a reason to
// segment, and it is worth a test that actually corrupts a file.
func TestDamageInASealedSegmentIsContained(t *testing.T) {
	dir := t.TempDir()
	l := openWith(t, dir, Options{SegmentBytes: 1 << 10, MaxIndexed: -1})
	fill(t, l, "s1", 400)
	segments := l.Stats().Segments
	if segments < 3 {
		t.Fatalf("fixture needs at least 3 segments, got %d", segments)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Corrupt the *first* segment, which is sealed. A single file's worth of
	// records is the most this may cost.
	first := filepath.Join(dir, fmt.Sprintf("%s%06d%s", segPrefix, 0, segSuffix))
	raw, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read first segment: %v", err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(first, raw, 0o644); err != nil {
		t.Fatalf("write first segment: %v", err)
	}

	l2 := openWith(t, dir, Options{SegmentBytes: 1 << 10, MaxIndexed: -1})
	st := l2.Stats()
	if len(st.Damage) == 0 {
		t.Fatal("an altered sealed segment was accepted silently")
	}
	got, _ := l2.Session("s1", 0)
	if len(got) == 0 {
		t.Fatal("damage in one segment cost the whole log")
	}
	if len(got) >= 400 {
		t.Fatal("an altered record was served as genuine")
	}

	// Containment is the claim, so this is the assertion that matters: the
	// records living in the segments *after* the damaged one must still be
	// there. Had framing desynchronised — which is exactly what a single-file
	// log does — the loss would run from the corruption to the end, and the
	// last record would be an early one.
	seen := make(map[string]bool, len(got))
	for _, e := range got {
		seen[e.MsgID] = true
	}
	if !seen["m399"] {
		t.Error("the last record is gone: damage in the first segment cost the segments after it")
	}
	// And the loss must be bounded by the damaged segment: the records the
	// first segment never held are all still readable.
	missing := 0
	for i := 0; i < 400; i++ {
		if !seen[fmt.Sprintf("m%d", i)] {
			missing++
		}
	}
	perSegment := 400/segments + 1
	if missing > perSegment {
		t.Errorf("%d records lost to a corrupt segment holding about %d — the damage was not contained",
			missing, perSegment)
	}
}

// The bound that matters most: memory must not grow with history.
func TestIndexMemoryIsBoundedByOptionNotByHistory(t *testing.T) {
	dir := t.TempDir()
	l := openWith(t, dir, Options{SegmentBytes: 8 << 10, MaxIndexed: 64})
	for s := 0; s < 40; s++ {
		fill(t, l, fmt.Sprintf("s%d", s), 50) // 2000 records
	}
	if got := l.Stats().Indexed; got > 64 {
		t.Fatalf("2000 records left %d locators indexed against a cap of 64", got)
	}
}
