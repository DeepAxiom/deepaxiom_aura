package demo

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The pieces of the demo that are instruments: the things whose readings end
// up on the screen as facts. If one of them is wrong, the demo is wrong while
// looking exactly as convincing — so they are tested on their own, away from
// any node. Whether a node survives the six beats is checked by running
// `aura demo --headless` against a real binary, in CI.

func TestRecorderReportsWhatWasMeasuredInAWindow(t *testing.T) {
	var r recorder
	start := time.Now()

	for i := 1; i <= 100; i++ { // 1..100 ms
		r.add(float32(i))
	}
	first := start.Add(250 * time.Millisecond)
	if tk := r.roll(first); len(tk.lat) != 100 || tk.lat[0] != 1 || tk.lat[99] != 100 {
		t.Fatalf("the first tick holds %d samples, from %v to %v", len(tk.lat), tk.lat[0], tk.lat[len(tk.lat)-1])
	}
	for i := 0; i < 50; i++ {
		r.add(1000)
	}
	second := start.Add(500 * time.Millisecond)
	r.roll(second)

	w := r.between(start, first)
	if w.N != 100 || w.P50 != 51 || w.P99 != 100 {
		t.Errorf("first window: n=%d p50=%v p99=%v, want 100, 51, 100", w.N, w.P50, w.P99)
	}
	if w.Rate != 400 {
		t.Errorf("100 samples in a quarter of a second is 400/s, got %v", w.Rate)
	}
	// A window takes the ticks that closed inside it and no others.
	if w := r.between(first, second); w.N != 50 || w.P50 != 1000 {
		t.Errorf("second window: n=%d p50=%v, want 50 samples of 1000", w.N, w.P50)
	}
	if w := r.between(second, second.Add(time.Second)); w.N != 0 || w.P99 != 0 {
		t.Errorf("an empty window reported n=%d p99=%v", w.N, w.P99)
	}
}

func TestRecorderForgetsWhatIsOlderThanItKeeps(t *testing.T) {
	var r recorder
	at := time.Now()
	for i := 0; i < keepTicks+25; i++ {
		r.add(1)
		at = at.Add(tickEvery)
		r.roll(at)
	}
	if len(r.ticks) != keepTicks {
		t.Fatalf("the recorder kept %d ticks, want %d", len(r.ticks), keepTicks)
	}
}

// What makes "the engine was released N ms after the cancel" a measurement
// rather than a claim is that the engine itself notices its caller leaving.
func TestTheSimulatedEngineKnowsWhenItsCallerLeft(t *testing.T) {
	e, err := newSimEngine("engine-key")
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := `{"model":"simulated","stream":true,"max_tokens":5000,"messages":[{"role":"user","content":"a long answer"}]}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.url()+"/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer engine-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the engine answered %s", resp.Status)
	}

	tokens := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && tokens < 20 {
		if strings.Contains(sc.Text(), `"content":"tok "`) {
			tokens++
		}
	}
	if tokens < 20 {
		t.Fatalf("only %d tokens arrived", tokens)
	}
	if got := e.waitReleased([]string{"a long answer"}, 50*time.Millisecond); len(got) != 0 {
		t.Fatal("the engine reported a release while its caller was still reading")
	}

	hungUp := time.Now()
	cancel()
	got := e.waitReleased([]string{"a long answer"}, 2*time.Second)
	rel, ok := got["a long answer"]
	if !ok {
		t.Fatal("the engine never noticed its caller had gone")
	}
	if rel.produced < 20 || rel.produced >= rel.planned || rel.planned != 5000 {
		t.Errorf("released after %d of %d tokens", rel.produced, rel.planned)
	}
	if late := rel.at.Sub(hungUp); late < 0 || late > time.Second {
		t.Errorf("the release was recorded %s after the caller hung up", late)
	}
	if e.active.Load() != 0 {
		t.Errorf("the engine still counts %d generations in progress", e.active.Load())
	}
}

func TestTheSimulatedEngineReportsUsageAndWantsItsKey(t *testing.T) {
	e, err := newSimEngine("engine-key")
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()

	post := func(key string) (*http.Response, string) {
		body := `{"stream":true,"max_tokens":3,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest(http.MethodPost, e.url()+"/chat/completions", strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			b.WriteString(sc.Text() + "\n")
		}
		return resp, b.String()
	}

	if resp, _ := post(""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a call with no key answered %s", resp.Status)
	}
	if resp, _ := post("someone-else's"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a call with the wrong key answered %s", resp.Status)
	}
	resp, stream := post("engine-key")
	if resp.StatusCode != 200 {
		t.Fatalf("a call with the key answered %s", resp.Status)
	}
	if n := strings.Count(stream, `"content":"tok "`); n != 3 {
		t.Errorf("asked for 3 tokens, the stream carries %d", n)
	}
	if !strings.Contains(stream, `"completion_tokens":3`) || !strings.Contains(stream, `"cached_tokens":24`) {
		t.Errorf("the stream does not end with the usage that was asked for:\n%s", stream)
	}
	if !strings.HasSuffix(strings.TrimSpace(stream), "data: [DONE]") {
		t.Errorf("the stream does not end with [DONE]")
	}
	if e.refused.Load() != 2 {
		t.Errorf("the engine counted %d refused calls, want 2", e.refused.Load())
	}
}

// The service is what the attack is measured against, so it must do exactly
// one thing: carry out an order when the key is right, and only then.
func TestTheServiceCarriesOutAnOrderOnlyWithItsKey(t *testing.T) {
	s, err := newService()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	order := func(id, key string) int {
		req, _ := http.NewRequest(http.MethodPost, s.url()+"/orders", strings.NewReader(`{"id":"`+id+`"}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := order("a", ""); code != http.StatusUnauthorized {
		t.Errorf("no key: %d", code)
	}
	if code := order("b", newKey()); code != http.StatusUnauthorized {
		t.Errorf("a guessed key: %d", code)
	}
	if code := order("c", s.key); code != 200 {
		t.Errorf("the right key: %d", code)
	}
	if s.carriedOut("a") || s.carriedOut("b") || !s.carriedOut("c") {
		t.Errorf("carried out a=%v b=%v c=%v, want only c", s.carriedOut("a"), s.carriedOut("b"), s.carriedOut("c"))
	}
	if s.executed.Load() != 1 || s.rejected.Load() != 2 {
		t.Errorf("executed %d, rejected %d; want 1 and 2", s.executed.Load(), s.rejected.Load())
	}
}

// The tampering has to be the smallest edit that changes meaning: one byte,
// inside a value, leaving a document that still parses. An edit that broke the
// JSON would be caught by anything, and would prove nothing about the chain.
func TestTamperChangesOneByteAndLeavesTheEntryWellFormed(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ledger_entries (
		seq INTEGER PRIMARY KEY, hash TEXT NOT NULL UNIQUE, session TEXT NOT NULL,
		entry TEXT NOT NULL, ts INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	original := `{"seq":7,"prev":"sha256:aa","session":"s","capability":"motor.demo.dispatch","outcome":"delivered"}`
	if _, err := db.Exec(`INSERT INTO ledger_entries VALUES (7, 'sha256:bb', 's', ?, 1)`, original); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ed, err := tamper(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	if ed.field != "capability" || ed.from != "motor.demo.dispatch" || ed.to != "motor.demo.dispatcx" {
		t.Errorf("the edit is reported as %+v", ed)
	}

	db, _ = sql.Open("sqlite", filepath.Join(dir, "kernel.db"))
	defer db.Close()
	var after string
	if err := db.QueryRow(`SELECT entry FROM ledger_entries WHERE seq = 7`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if len(after) != len(original) {
		t.Fatalf("the entry changed length: %d to %d bytes", len(original), len(after))
	}
	differing := 0
	for i := range original {
		if original[i] != after[i] {
			differing++
		}
	}
	if differing != 1 {
		t.Errorf("%d bytes differ, want exactly one", differing)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(after), &parsed); err != nil {
		t.Fatalf("the edited entry no longer parses: %v", err)
	}
	if parsed["capability"] != "motor.demo.dispatcx" {
		t.Errorf("the edited entry's capability is %v", parsed["capability"])
	}

	if _, err := tamper(dir, 99); err == nil {
		t.Error("tampering with an entry that does not exist reported success")
	}
}

func TestARunSurvivedOnlyIfEveryBeatDid(t *testing.T) {
	all := func(status string) []Beat {
		var out []Beat
		for _, id := range beatOrder {
			out = append(out, Beat{ID: id, Status: status})
		}
		return out
	}
	if !(Snapshot{Phase: "done", Beats: all("survived")}).Survived() {
		t.Error("six survived beats in a finished run did not count as survived")
	}
	if (Snapshot{Phase: "running", Beats: all("survived")}).Survived() {
		t.Error("a run that has not finished counted as survived")
	}
	one := all("survived")
	one[3].Status = "failed"
	if (Snapshot{Phase: "done", Beats: one}).Survived() {
		t.Error("a run with a failed beat counted as survived")
	}
	skipped := all("survived")
	skipped[5].Status = "pending"
	if (Snapshot{Phase: "done", Beats: skipped}).Survived() {
		t.Error("a run with a beat that never ran counted as survived")
	}
}

// A beat with no checks proved nothing, and must not be shown as survived.
func TestABeatThatCheckedNothingDidNotSurvive(t *testing.T) {
	r := New(Options{Sessions: 10})
	r.runBeat(0, func(*beat) {})
	if got := r.Snapshot().Beats[0].Status; got != "failed" {
		t.Errorf("a beat that checked nothing is %q", got)
	}
	r.runBeat(1, func(b *beat) { b.check("held", true, "") })
	if got := r.Snapshot().Beats[1].Status; got != "survived" {
		t.Errorf("a beat whose one check held is %q", got)
	}
	r.runBeat(2, func(b *beat) { b.check("held", true, ""); b.check("broke", false, "it did not") })
	if got := r.Snapshot().Beats[2]; got.Status != "failed" || got.Checks[1].Detail != "it did not" {
		t.Errorf("a beat with a failed check is %+v", got)
	}
	r.runBeat(3, func(*beat) { panic("boom") })
	if got := r.Snapshot().Beats[3].Status; got != "failed" {
		t.Errorf("a beat that panicked is %q", got)
	}
}

func TestTheScreenIsToldWhatEachBeatWillDoBeforeItDoes(t *testing.T) {
	r := New(Options{Sessions: 120})
	beats := r.Snapshot().Beats
	if len(beats) != len(beatOrder) {
		t.Fatalf("%d beats, want %d", len(beats), len(beatOrder))
	}
	for i, b := range beats {
		if b.ID != beatOrder[i] || b.Status != "pending" {
			t.Errorf("beat %d is %s/%s", i, b.ID, b.Status)
		}
	}
	if got := beats[0].Facts["sessions"]; got != slowSessions(120) {
		t.Errorf("the slow beat says %v sessions, want %d", got, slowSessions(120))
	}
	if beats[2].Facts["deadline_ms"] != lateDeadlineMS || beats[3].Facts["generations"] != cutGens {
		t.Errorf("the plans are %v and %v", beats[2].Facts, beats[3].Facts)
	}
}
