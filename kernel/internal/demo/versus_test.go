package demo

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The comparison is only worth showing if the agent on its left is a fair one.
// These pin what it is: it does have a rule, a memory and a log check, they do
// work, and the four things the comparison says about it are true of the code
// rather than of a recording.

type plainHarness struct {
	t     *testing.T
	svc   *service
	srv   *httptest.Server
	log   string
	agent *plainAgent
}

func newPlainHarness(t *testing.T) *plainHarness {
	t.Helper()
	svc, err := newService()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.close)
	h := &plainHarness{t: t, svc: svc, log: filepath.Join(t.TempDir(), "audit.log")}
	h.start()
	return h
}

// start brings the agent up over the same log, as a restart does.
func (h *plainHarness) start() {
	h.t.Helper()
	agent, handler, err := newPlainAgent(PlainConfig{ServiceURL: h.svc.url(), ServiceKey: h.svc.key, LogPath: h.log})
	if err != nil {
		h.t.Fatal(err)
	}
	h.agent = agent
	h.srv = httptest.NewServer(handler)
	h.t.Cleanup(h.srv.Close)
}

func (h *plainHarness) ask(session string, m plainMsg) plainReply {
	h.t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.srv.URL, "http")+"/session?id="+session, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(m); err != nil {
		h.t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var r plainReply
	if err := conn.ReadJSON(&r); err != nil {
		h.t.Fatalf("no answer: %v", err)
	}
	return r
}

func (h *plainHarness) check() (ok bool, entries int) {
	h.t.Helper()
	resp, err := http.Get(h.srv.URL + "/check")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		OK      bool `json:"ok"`
		Entries int  `json:"entries"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.OK, out.Entries
}

func TestThePlainAgentHasARuleAndItHoldsAtTheFrontDoor(t *testing.T) {
	h := newPlainHarness(t)

	if r := h.ask("s", plainMsg{ID: "1", Op: "work", Order: "ship-1"}); r.Done != "ship-1" || r.Turn != 1 {
		t.Fatalf("an allowed order was answered %+v", r)
	}
	if r := h.ask("s", plainMsg{ID: "2", Op: "work", Order: forbiddenPrefix + "1"}); r.Error == "" || r.Done != "" {
		t.Fatalf("a forbidden order was answered %+v", r)
	}
	if !h.svc.carriedOut("ship-1") || h.svc.carriedOut(forbiddenPrefix+"1") {
		t.Fatalf("the service carried out ship-1=%v, the payment=%v",
			h.svc.carriedOut("ship-1"), h.svc.carriedOut(forbiddenPrefix+"1"))
	}
}

// The first thing the comparison says: taken over from inside, the agent's key
// is there to be used and its rule is not on the way.
func TestTakenOverThePlainAgentsKeyCarriesOutWhatItsRuleForbids(t *testing.T) {
	h := newPlainHarness(t)
	orders := []string{forbiddenPrefix + "a", forbiddenPrefix + "b"}
	body, _ := json.Marshal(map[string]any{"orders": orders})
	resp, err := http.Post(h.srv.URL+"/taken-over", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, o := range orders {
		if !h.svc.carriedOut(o) {
			t.Errorf("%s was not carried out", o)
		}
	}
	// And nothing of it is in the log the agent would be audited by.
	if _, entries := h.check(); entries != 0 {
		t.Errorf("the log holds %d entries for orders the agent never recorded", entries)
	}
}

// The third thing: the agent does remember what it has done, so a request
// repeated to the same process is not carried out twice — and a request
// repeated to the process that replaced it is.
func TestThePlainAgentRemembersARequestUntilItIsRestarted(t *testing.T) {
	h := newPlainHarness(t)
	first := h.ask("s", plainMsg{ID: "req-1", Op: "work", Order: "ship-7"})
	again := h.ask("s", plainMsg{ID: "req-1", Op: "work", Order: "ship-7"})
	if first.Done != "ship-7" || again.Done != "ship-7" {
		t.Fatalf("answers were %+v and %+v", first, again)
	}
	if n := h.svc.times("ship-7"); n != 1 {
		t.Fatalf("asked twice of one process, carried out %d times", n)
	}

	h.srv.Close()
	h.start() // the supervisor's restart: same log, new process
	after := h.ask("s", plainMsg{ID: "req-1", Op: "work", Order: "ship-7"})
	if after.Done != "ship-7" {
		t.Fatalf("the restarted agent answered %+v", after)
	}
	if n := h.svc.times("ship-7"); n != 2 {
		t.Errorf("asked again of the restarted agent, carried out %d times in all, want 2", n)
	}
	if after.Turn != 1 {
		t.Errorf("the restarted agent says this session is at turn %d; it should have forgotten it", after.Turn)
	}
}

// The second thing: an order issued late is carried out, because nothing on
// the way knows how old the decision is.
func TestThePlainAgentCarriesOutAnOrderHoweverLateItIs(t *testing.T) {
	h := newPlainHarness(t)
	start := time.Now()
	r := h.ask("s", plainMsg{ID: "late-1", Op: "late", Order: "ship-late", DelayMS: 120})
	if r.Done != "ship-late" || !h.svc.carriedOut("ship-late") {
		t.Fatalf("the late order was answered %+v", r)
	}
	if took := time.Since(start); took < 120*time.Millisecond {
		t.Errorf("the order was carried out after %s, before its delay", took)
	}
}

// The fourth thing: the agent's check of its own log is a real check — it
// catches a log that is damaged — and it cannot catch one that was edited to
// say something else.
func TestThePlainAgentsLogCheckCatchesDamageAndNotAnEdit(t *testing.T) {
	h := newPlainHarness(t)
	for i, order := range []string{"ship-1", "ship-2", "ship-3", "ship-4", "ship-5"} {
		h.ask("s", plainMsg{ID: "r" + order, Op: "work", Order: order})
		if ok, entries := h.check(); !ok || entries != i+1 {
			t.Fatalf("after %d orders the check says ok=%v entries=%d", i+1, ok, entries)
		}
	}
	before, _ := os.ReadFile(h.log)

	ed, line, err := tamperPlain(h.log)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(h.log)
	differing := 0
	for i := range before {
		if before[i] != after[i] {
			differing++
		}
	}
	if len(before) != len(after) || differing != 1 {
		t.Fatalf("the edit changed %d bytes and the length from %d to %d", differing, len(before), len(after))
	}
	if line != 3 || ed.from != "ship-3" || ed.to != "ship-x" {
		t.Errorf("the edit is reported as line %d, %+v", line, ed)
	}
	if ok, entries := h.check(); !ok || entries != 5 {
		t.Errorf("the check noticed the edit (ok=%v, entries=%d); the comparison says it cannot", ok, entries)
	}

	// Damage, as opposed to an edit: a line that is no longer a record.
	if err := os.WriteFile(h.log, append(after[:len(after)/2], []byte("\n{not a record}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := h.check(); ok {
		t.Error("the check passed a log with a line that is not a record")
	}
}

// A beat is held only if the node's checks passed and nothing reached it, and
// the agent is harmed only if something did happen to it.
func TestAComparisonBeatIsJudgedByWhatHappenedOnEachSide(t *testing.T) {
	v := NewVersus(Options{})
	v.runBeat(0, func(b *vbeat) { b.check("held", true, ""); b.harm(sidePlain, 20) })
	v.runBeat(1, func(b *vbeat) { b.check("held", true, "") })
	v.runBeat(2, func(b *vbeat) { b.check("held", true, ""); b.harm(sideAura, 1); b.harm(sidePlain, 3) })
	v.runBeat(3, func(b *vbeat) { b.check("broke", false, "it did not"); b.harm(sidePlain, 1) })

	s := v.Snapshot()
	want := []struct{ harmed, held bool }{{true, true}, {false, true}, {true, false}, {true, false}}
	for i, w := range want {
		if s.Beats[i].Harmed != w.harmed || s.Beats[i].Held != w.held {
			t.Errorf("beat %d: harmed=%v held=%v, want %v and %v", i, s.Beats[i].Harmed, s.Beats[i].Held, w.harmed, w.held)
		}
	}
	if s.Plain.Harm != 24 || s.Aura.Harm != 1 {
		t.Errorf("harm counted as %d and %d", s.Plain.Harm, s.Aura.Harm)
	}
}

// A run passes only when it came out the way the comparison is described.
func TestAComparisonPassesOnlyIfItCameOutAsDescribed(t *testing.T) {
	judge := func(held, harmed bool) bool {
		v := NewVersus(Options{})
		for i := range v.beats {
			v.beats[i].Status, v.beats[i].Held, v.beats[i].Harmed = "done", true, true
		}
		v.beats[2].Held, v.beats[2].Harmed = held, harmed
		v.phase = "done"
		return v.Survived()
	}
	if !judge(true, true) {
		t.Error("a run where the node held and the agent was harmed did not pass")
	}
	if judge(false, true) {
		t.Error("a run where the node did not hold passed")
	}
	if judge(true, false) {
		t.Error("a run where the agent was not harmed passed, though the comparison says it is")
	}
	if got := versusPlan("late")["deadline_ms"]; got != lateDeadlineMS {
		t.Errorf("the late beat's plan says %v ms", got)
	}
}
