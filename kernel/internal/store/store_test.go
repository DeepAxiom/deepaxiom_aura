package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// migrate() runs on every Open, so reopening an existing data dir must be a
// no-op rather than an error — that is what makes a node restartable.
func TestMigrationsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.SaveGraph("g", []byte(`{"ir":"1"}`)); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	ir, err := second.LoadGraph("g")
	if err != nil {
		t.Fatalf("data must survive a reopen: %v", err)
	}
	if string(ir) != `{"ir":"1"}` {
		t.Fatalf("graph came back as %q", ir)
	}
}

func TestGraphSaveLoadListAndOverwrite(t *testing.T) {
	st := open(t)

	if _, err := st.LoadGraph("missing"); err == nil {
		t.Fatal("want an error for an unknown graph id")
	}

	if err := st.SaveGraph("chat", []byte(`{"v":1}`)); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}
	if err := st.SaveGraph("echo", []byte(`{"v":1}`)); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// Registering the same id again replaces the IR rather than erroring.
	if err := st.SaveGraph("chat", []byte(`{"v":2}`)); err != nil {
		t.Fatalf("re-SaveGraph: %v", err)
	}
	ir, err := st.LoadGraph("chat")
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if string(ir) != `{"v":2}` {
		t.Fatalf("want the graph overwritten, got %q", ir)
	}

	ids, err := st.ListGraphs()
	if err != nil {
		t.Fatalf("ListGraphs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 graphs, got %v", ids)
	}
}

func TestSessionLifecycleAndListing(t *testing.T) {
	st := open(t)

	if _, err := st.GetSession("nope"); err == nil {
		t.Fatal("want an error for an unknown session")
	}

	if err := st.StartSession("s1", "chat"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	meta, err := st.GetSession("s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if meta.GraphID != "chat" {
		t.Fatalf("graph = %q, want chat", meta.GraphID)
	}
	if meta.Started == 0 {
		t.Fatal("want a start timestamp")
	}
	if meta.Ended != 0 {
		t.Fatal("a live session must not report an end time")
	}

	if err := st.EndSession("s1"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	meta, _ = st.GetSession("s1")
	if meta.Ended == 0 {
		t.Fatal("want an end timestamp after EndSession")
	}
}

// The event log is the raw material behind `aura why` and `aura replay`: it
// must come back in insertion order, with timestamps aligned to the entries.
func TestEventLogPreservesOrderAndTimestamps(t *testing.T) {
	st := open(t)
	if err := st.StartSession("s1", "chat"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	for _, e := range []struct{ id, cause, body string }{
		{"m1", "", `{"kind":"data","n":1}`},
		{"m2", "m1", `{"kind":"data","n":2}`},
		{"m3", "m2", `{"kind":"error","n":3}`},
	} {
		if err := st.AppendEvent("s1", e.id, e.cause, []byte(e.body)); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	if err := st.AppendEvent("other", "x1", "", []byte(`{"kind":"data"}`)); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	events, times, err := st.SessionEvents("s1", 0)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("want only this session's 3 events, got %d", len(events))
	}
	if len(times) != len(events) {
		t.Fatalf("timestamps (%d) must be index-aligned with events (%d)", len(times), len(events))
	}
	for i, want := range []float64{1, 2, 3} {
		var got struct {
			N float64 `json:"n"`
		}
		if err := json.Unmarshal(events[i], &got); err != nil {
			t.Fatalf("event %d is not valid json: %v", i, err)
		}
		if got.N != want {
			t.Fatalf("event %d is n=%v, want n=%v — log order is not insertion order", i, got.N, want)
		}
		if times[i] == 0 {
			t.Fatalf("event %d has no timestamp", i)
		}
	}
}

// ListSessions counts errors so the UI can badge a failed session without
// reading its whole log.
func TestListSessionsCountsEventsAndErrors(t *testing.T) {
	st := open(t)
	if err := st.StartSession("s1", "chat"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	_ = st.AppendEvent("s1", "m1", "", []byte(`{"kind":"data"}`))
	_ = st.AppendEvent("s1", "m2", "m1", []byte(`{"kind":"error"}`))
	_ = st.AppendEvent("s1", "m3", "m2", []byte(`{"kind":"error"}`))

	list, err := st.ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 session, got %d", len(list))
	}
	if list[0].Events != 3 {
		t.Fatalf("events = %d, want 3", list[0].Events)
	}
	if list[0].Errors != 2 {
		t.Fatalf("errors = %d, want 2", list[0].Errors)
	}
}

func TestSkillConfigRoundTrip(t *testing.T) {
	st := open(t)

	// Never-configured skills read as empty, not as an error: the caller
	// merges this over the manifest defaults.
	vals, err := st.LoadSkillConfig("acme/x/y")
	if err != nil {
		t.Fatalf("LoadSkillConfig on an unknown skill: %v", err)
	}
	if len(vals) != 0 {
		t.Fatalf("want an empty map, got %v", vals)
	}

	if err := st.SaveSkillConfig("acme/x/y", map[string]any{"temperature": 0.3, "mode": "live"}); err != nil {
		t.Fatalf("SaveSkillConfig: %v", err)
	}
	vals, err = st.LoadSkillConfig("acme/x/y")
	if err != nil {
		t.Fatalf("LoadSkillConfig: %v", err)
	}
	if vals["temperature"] != 0.3 || vals["mode"] != "live" {
		t.Fatalf("round trip lost values: %v", vals)
	}

	// Saving replaces the whole set — callers merge before calling.
	if err := st.SaveSkillConfig("acme/x/y", map[string]any{"mode": "disabled"}); err != nil {
		t.Fatalf("re-SaveSkillConfig: %v", err)
	}
	vals, _ = st.LoadSkillConfig("acme/x/y")
	if _, still := vals["temperature"]; still {
		t.Fatalf("want the value set replaced, got %v", vals)
	}
}

func TestUpsertSkillAndLoadManifest(t *testing.T) {
	st := open(t)

	if _, err := st.LoadSkillManifest("acme/x/y"); err == nil {
		t.Fatal("want an error for a skill this node has never seen")
	}

	m := map[string]any{"id": "acme/x/y", "version": "1.0.0", "capability": "logical.x"}
	if err := st.UpsertSkill("acme/x/y", "1.0.0", m); err != nil {
		t.Fatalf("UpsertSkill: %v", err)
	}
	// Reconnecting the same version updates rather than duplicating.
	if err := st.UpsertSkill("acme/x/y", "1.0.0", m); err != nil {
		t.Fatalf("re-UpsertSkill: %v", err)
	}

	raw, err := st.LoadSkillManifest("acme/x/y")
	if err != nil {
		t.Fatalf("LoadSkillManifest: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("stored manifest is not valid json: %v", err)
	}
	if got["capability"] != "logical.x" {
		t.Fatalf("manifest came back as %v", got)
	}
}

// Unlike projections, an inbound route must be removable: a webhook URL you
// cannot revoke is a security problem in itself.
func TestIngressSaveLoadAndDelete(t *testing.T) {
	st := open(t)

	all, err := st.LoadIngress()
	if err != nil {
		t.Fatalf("LoadIngress on an empty store: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("want no routes, got %d", len(all))
	}

	if err := st.SaveIngress("stripe", []byte(`{"name":"stripe","graph":"billing"}`)); err != nil {
		t.Fatalf("SaveIngress: %v", err)
	}
	if err := st.SaveIngress("gh", []byte(`{"name":"gh","graph":"ci"}`)); err != nil {
		t.Fatalf("SaveIngress: %v", err)
	}
	// Re-declaring a route replaces it rather than erroring.
	if err := st.SaveIngress("stripe", []byte(`{"name":"stripe","graph":"invoices"}`)); err != nil {
		t.Fatalf("re-SaveIngress: %v", err)
	}

	all, err = st.LoadIngress()
	if err != nil {
		t.Fatalf("LoadIngress: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 routes after an upsert, got %d", len(all))
	}
	if string(all["stripe"]) != `{"name":"stripe","graph":"invoices"}` {
		t.Fatalf("want the route overwritten, got %s", all["stripe"])
	}

	if err := st.DeleteIngress("stripe"); err != nil {
		t.Fatalf("DeleteIngress: %v", err)
	}
	all, _ = st.LoadIngress()
	if _, still := all["stripe"]; still {
		t.Fatal("the revoked route is still stored")
	}
	if _, other := all["gh"]; !other {
		t.Fatal("revoking one route removed another")
	}

	// Deleting something that is not there is not an error — revocation
	// should be safe to repeat.
	if err := st.DeleteIngress("stripe"); err != nil {
		t.Fatalf("second DeleteIngress: %v", err)
	}
}

func TestProjectionSaveAndLoad(t *testing.T) {
	st := open(t)

	if err := st.SaveProjection("crm", []byte(`{"name":"crm","ops":[]}`)); err != nil {
		t.Fatalf("SaveProjection: %v", err)
	}
	if err := st.SaveProjection("crm", []byte(`{"name":"crm","ops":[1]}`)); err != nil {
		t.Fatalf("re-SaveProjection: %v", err)
	}

	all, err := st.LoadProjections()
	if err != nil {
		t.Fatalf("LoadProjections: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 projection after an upsert, got %d", len(all))
	}
	if string(all["crm"]) != `{"name":"crm","ops":[1]}` {
		t.Fatalf("want the config overwritten, got %s", all["crm"])
	}
}

// --- the effect ledger (C4) --------------------------------------------------
//
// The store only persists rows here; the chain-integrity rules live in
// kernel/internal/ledger and are tested there. What belongs to the store is
// the plumbing: does an empty ledger report itself as empty, does ordering
// hold, does a duplicate hash get rejected.

func TestLedgerHeadOfAnEmptyLedgerIsGenesis(t *testing.T) {
	st := open(t)
	seq, hash, err := st.LedgerHead()
	if err != nil {
		t.Fatalf("LedgerHead: %v", err)
	}
	if seq != 0 || hash != "" {
		t.Fatalf("empty ledger head = (%d, %q); want (0, \"\")", seq, hash)
	}
	n, err := st.LedgerEntryCount()
	if err != nil {
		t.Fatalf("LedgerEntryCount: %v", err)
	}
	if n != 0 {
		t.Fatalf("empty ledger reports %d entries", n)
	}
}

func TestLedgerHeadTracksTheLastAppend(t *testing.T) {
	st := open(t)
	if err := st.AppendLedgerEntry(1, "hash-1", "sess-a", []byte(`{"seq":1}`)); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := st.AppendLedgerEntry(2, "hash-2", "sess-a", []byte(`{"seq":2}`)); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	seq, hash, err := st.LedgerHead()
	if err != nil {
		t.Fatalf("LedgerHead: %v", err)
	}
	if seq != 2 || hash != "hash-2" {
		t.Fatalf("head = (%d, %q); want (2, \"hash-2\")", seq, hash)
	}
	n, err := st.LedgerEntryCount()
	if err != nil {
		t.Fatalf("LedgerEntryCount: %v", err)
	}
	if n != 2 {
		t.Fatalf("count = %d; want 2", n)
	}
}

// A duplicate hash is a bug, not a user action — the UNIQUE constraint turns
// it into a loud failure instead of a silently forked chain.
func TestDuplicateLedgerHashIsRejected(t *testing.T) {
	st := open(t)
	if err := st.AppendLedgerEntry(1, "hash-1", "sess-a", []byte(`{}`)); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := st.AppendLedgerEntry(2, "hash-1", "sess-a", []byte(`{}`)); err == nil {
		t.Fatal("a duplicate hash was accepted")
	}
}

func TestLedgerEntriesReturnsInSeqOrderFromAPoint(t *testing.T) {
	st := open(t)
	for i := uint64(1); i <= 5; i++ {
		if err := st.AppendLedgerEntry(i, fmt.Sprintf("hash-%d", i), "sess-a",
			[]byte(fmt.Sprintf(`{"seq":%d}`, i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	all, err := st.LedgerEntries(1, 0)
	if err != nil {
		t.Fatalf("LedgerEntries: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("got %d entries; want 5", len(all))
	}

	fromThree, err := st.LedgerEntries(3, 0)
	if err != nil {
		t.Fatalf("LedgerEntries(3): %v", err)
	}
	if len(fromThree) != 3 {
		t.Fatalf("got %d entries from seq 3; want 3", len(fromThree))
	}
	var first struct {
		Seq int `json:"seq"`
	}
	if err := json.Unmarshal(fromThree[0], &first); err != nil || first.Seq != 3 {
		t.Fatalf("first entry from seq 3 is %s; want seq 3", fromThree[0])
	}

	limited, err := st.LedgerEntries(1, 2)
	if err != nil {
		t.Fatalf("LedgerEntries with limit: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("got %d entries with limit 2; want 2", len(limited))
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	st := open(t)

	if _, ok, err := st.LastCheckpoint(); err != nil || ok {
		t.Fatalf("an empty ledger reported a checkpoint: ok=%v err=%v", ok, err)
	}

	cp1 := CheckpointRow{Seq: 10, HeadHash: "hash-10", Pubkey: "pub", Signature: "sig-1", TS: 1000}
	cp2 := CheckpointRow{Seq: 20, HeadHash: "hash-20", Pubkey: "pub", Signature: "sig-2", TS: 2000}
	if err := st.SaveCheckpoint(cp1); err != nil {
		t.Fatalf("SaveCheckpoint 1: %v", err)
	}
	if err := st.SaveCheckpoint(cp2); err != nil {
		t.Fatalf("SaveCheckpoint 2: %v", err)
	}

	all, err := st.LedgerCheckpoints()
	if err != nil {
		t.Fatalf("LedgerCheckpoints: %v", err)
	}
	if len(all) != 2 || all[0].Seq != 10 || all[1].Seq != 20 {
		t.Fatalf("checkpoints out of order or missing: %+v", all)
	}

	last, ok, err := st.LastCheckpoint()
	if err != nil || !ok {
		t.Fatalf("LastCheckpoint: ok=%v err=%v", ok, err)
	}
	if last.Seq != 20 || last.Signature != "sig-2" {
		t.Fatalf("LastCheckpoint = %+v; want seq 20", last)
	}
}
