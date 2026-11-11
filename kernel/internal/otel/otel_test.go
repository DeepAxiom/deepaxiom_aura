package otel

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aura/kernel/internal/channel"
)

const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func attr(span map[string]any, key string) (string, bool) {
	for _, a := range span["attributes"].([]map[string]any) {
		if a["key"] == key {
			v := a["value"].(map[string]any)
			if s, ok := v["stringValue"]; ok {
				return s.(string), true
			}
			return v["intValue"].(string), true
		}
	}
	return "", false
}

func TestASessionWithoutContextKeepsItsDerivedTrace(t *testing.T) {
	e := channel.Envelope{ID: "E1", CauseID: "E0", Kind: "data", Node: "chat", Port: "text_out"}
	span := Span(SpanInput{Session: "s1", Envelope: e, TSMillis: 1700000000000, ParentKnown: true})

	if span["traceId"] != TraceID("s1") {
		t.Fatalf("trace id %v; a session with no context must keep the id it always had", span["traceId"])
	}
	if span["parentSpanId"] != SpanID("E0") || span["spanId"] != SpanID("E1") {
		t.Fatalf("span/parent ids changed: %v / %v", span["spanId"], span["parentSpanId"])
	}
	if span["name"] != "chat.text_out data" {
		t.Fatalf("span name %q", span["name"])
	}
	if span["startTimeUnixNano"] != "1700000000000000000" {
		t.Fatalf("start time %v", span["startTimeUnixNano"])
	}
	if _, has := attr(span, "aura.payload"); has {
		t.Fatal("a span carries a payload nobody asked it to")
	}
}

func TestAChainWithContextJoinsTheCallersTrace(t *testing.T) {
	root := Span(SpanInput{Session: "s1", Trace: tp,
		Envelope: channel.Envelope{ID: "E1", Kind: "data", Node: "client", Port: "text_out"}})
	if root["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id %v is not the caller's", root["traceId"])
	}
	if root["parentSpanId"] != "00f067aa0ba902b7" {
		t.Fatalf("the chain's first span hangs off %v, not the caller's span", root["parentSpanId"])
	}
	child := Span(SpanInput{Session: "s1", Trace: tp, ParentKnown: true,
		Envelope: channel.Envelope{ID: "E2", CauseID: "E1", Kind: "data"}})
	if child["parentSpanId"] != SpanID("E1") || child["traceId"] != root["traceId"] {
		t.Fatalf("a later span left the tree: parent %v trace %v", child["parentSpanId"], child["traceId"])
	}
}

func TestAnAttestedSpanCarriesModelAndUsage(t *testing.T) {
	e := channel.Envelope{ID: "E1", Kind: "data", Receipt: "abc123",
		Attest: json.RawMessage(`{"engine":"llama.cpp","model":"Qwen/Q","usage":{"input_tokens":900,"output_tokens":40,"cached_input_tokens":768}}`)}
	span := Span(SpanInput{Session: "s", Envelope: e})
	want := map[string]string{
		"gen_ai.system": "llama.cpp", "gen_ai.request.model": "Qwen/Q",
		"gen_ai.usage.input_tokens": "900", "gen_ai.usage.output_tokens": "40",
		"gen_ai.usage.cached_input_tokens": "768", "aura.receipt": "abc123",
	}
	for k, v := range want {
		if got, _ := attr(span, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// No usage reported: no usage attributes, rather than zeros that read as
	// "this reply cost nothing".
	bare := Span(SpanInput{Session: "s", Envelope: channel.Envelope{ID: "E2", Kind: "data",
		Attest: json.RawMessage(`{"engine":"e","model":"m"}`)}})
	if _, has := attr(bare, "gen_ai.usage.input_tokens"); has {
		t.Error("an attestation with no usage produced a usage attribute")
	}
}

func TestChainTracesFollowsCausesThroughSkillsThatDoNotEcho(t *testing.T) {
	events := []channel.Envelope{
		{ID: "c1", Trace: tp},                // client root, with context
		{ID: "k1", CauseID: "c1", Trace: tp}, // kernel forward
		{ID: "s1", CauseID: "k1"},            // an old SDK's reply: no trace field
		{ID: "k2", CauseID: "s1"},            // (as a pre-v1.7 kernel would have logged it)
		{ID: "c2"},                           // a second chain, no context
		{ID: "k3", CauseID: "c2"},
	}
	got := ChainTraces(events)
	for _, id := range []string{"c1", "k1", "s1", "k2"} {
		if got[id] != tp {
			t.Errorf("%s resolved to %q", id, got[id])
		}
	}
	for _, id := range []string{"c2", "k3"} {
		if got[id] != "" {
			t.Errorf("%s inherited a context from another chain: %q", id, got[id])
		}
	}
}

// ── the live exporter ──────────────────────────────────────────────────────

type collector struct {
	mu    sync.Mutex
	spans []map[string]any
	auth  []string
	srv   *httptest.Server
}

func newCollector(t *testing.T, status int) *collector {
	t.Helper()
	c := &collector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var doc struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []map[string]any `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		_ = json.Unmarshal(body, &doc)
		c.mu.Lock()
		c.auth = append(c.auth, r.Header.Get("X-Api-Key"))
		if status < 300 {
			for _, rs := range doc.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					c.spans = append(c.spans, ss.Spans...)
				}
			}
		}
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.spans)
}

func rawEnvelope(id, cause, session, payload string) []byte {
	raw, _ := json.Marshal(channel.Envelope{V: "1", ID: id, CauseID: cause, Session: session,
		Node: "chat", Port: "text_out", Kind: "data", Schema: "std/text@1",
		Payload: json.RawMessage(payload)})
	return raw
}

func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestExporterShipsSpansAndNeverPayloads(t *testing.T) {
	c := newCollector(t, 200)
	x, err := NewExporter(Config{Endpoint: c.srv.URL + "/", SampleRatio: 1, Version: "t",
		Headers: map[string]string{"X-Api-Key": "k"}, FlushEvery: 10 * time.Millisecond, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	secret := `{"text":"the patient is Jane Doe"}`
	x.Observe("s1", rawEnvelope("E1", "", "s1", secret), 1700000000000)
	x.Observe("s1", rawEnvelope("E2", "E1", "s1", secret), 1700000000005)
	until(t, "two spans", func() bool { return c.count() == 2 })
	x.Close()

	c.mu.Lock()
	defer c.mu.Unlock()
	all, _ := json.Marshal(c.spans)
	if strings.Contains(string(all), "Jane Doe") {
		t.Fatal("user content reached the telemetry backend")
	}
	if c.spans[1]["parentSpanId"] != SpanID("E1") {
		t.Fatalf("the second span's parent is %v", c.spans[1]["parentSpanId"])
	}
	if _, has := c.spans[0]["parentSpanId"]; has {
		t.Fatal("a root span was given a parent the backend never received")
	}
	if got, _ := attrAny(c.spans[0], "aura.session"); got != "s1" {
		t.Fatalf("aura.session = %q", got)
	}
	if c.auth[0] != "k" {
		t.Fatal("the configured header did not ride on the export")
	}
	if s := x.Stats(); s.Exported != 2 || s.Failed != 0 || s.Dropped != 0 {
		t.Fatalf("stats %+v", s)
	}
}

// attrAny reads an attribute from a span decoded from JSON.
func attrAny(span map[string]any, key string) (string, bool) {
	list, _ := span["attributes"].([]any)
	for _, a := range list {
		m := a.(map[string]any)
		if m["key"] == key {
			v := m["value"].(map[string]any)
			for _, inner := range v {
				return fmt.Sprint(inner), true
			}
		}
	}
	return "", false
}

func TestSkillRepliesInheritTheChainsTraceInTheLiveExport(t *testing.T) {
	c := newCollector(t, 200)
	x, _ := NewExporter(Config{Endpoint: c.srv.URL, SampleRatio: 1, FlushEvery: 10 * time.Millisecond, Log: quiet()})
	root, _ := json.Marshal(channel.Envelope{V: "1", ID: "c1", Session: "s", Node: "client", Kind: "data", Trace: tp})
	x.Observe("s", root, 1)
	x.Observe("s", rawEnvelope("s1", "c1", "s", `{}`), 2) // no trace field of its own
	until(t, "two spans", func() bool { return c.count() == 2 })
	x.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, span := range c.spans {
		if span["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Fatalf("span %d is in trace %v, not the caller's", i, span["traceId"])
		}
	}
	if c.spans[0]["parentSpanId"] != "00f067aa0ba902b7" {
		t.Fatalf("root span parent %v", c.spans[0]["parentSpanId"])
	}
}

func TestSamplingKeepsOrDropsWholeSessions(t *testing.T) {
	c := newCollector(t, 200)
	x, _ := NewExporter(Config{Endpoint: c.srv.URL, SampleRatio: 0.25, FlushEvery: 10 * time.Millisecond, Log: quiet()})
	defer x.Close()

	kept := 0
	const sessions = 4000
	for i := 0; i < sessions; i++ {
		id := fmt.Sprintf("session-%d", i)
		first := x.Sampled(id)
		// The same session always gets the same answer, or its tree would
		// have holes.
		for j := 0; j < 3; j++ {
			if x.Sampled(id) != first {
				t.Fatalf("session %s was sampled inconsistently", id)
			}
		}
		if first {
			kept++
		}
	}
	if kept < sessions*20/100 || kept > sessions*30/100 {
		t.Fatalf("a 25%% ratio kept %d of %d sessions", kept, sessions)
	}

	all, _ := NewExporter(Config{Endpoint: c.srv.URL, SampleRatio: 1, Log: quiet()})
	none, _ := NewExporter(Config{Endpoint: c.srv.URL, SampleRatio: 0, Log: quiet()})
	over, _ := NewExporter(Config{Endpoint: c.srv.URL, SampleRatio: 7, Log: quiet()})
	defer all.Close()
	defer none.Close()
	defer over.Close()
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("s%d", i)
		if !all.Sampled(id) || none.Sampled(id) || !over.Sampled(id) {
			t.Fatalf("ratio bounds are wrong for %s", id)
		}
	}
	none.Observe("s", rawEnvelope("E", "", "s", `{}`), 1)
	if none.Stats().SampledOut != 1 {
		t.Fatal("a sampled-out envelope was not counted")
	}
}

// The property the whole design is for: a collector that never answers costs
// the delivery path nothing.
func TestAHungCollectorNeverBlocksTheNode(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	x, _ := NewExporter(Config{Endpoint: srv.URL, SampleRatio: 1, QueueSize: 64, BatchSize: 8,
		FlushEvery: 5 * time.Millisecond, Log: quiet(),
		Client: &http.Client{Timeout: 30 * time.Second}})

	// Built once: this measures Observe, not the test's own JSON encoding.
	raw := rawEnvelope("E", "", "s", `{}`)
	start := time.Now()
	for i := 0; i < 50000; i++ {
		x.Observe("s", raw, int64(i))
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("50,000 Observe calls against a hung collector took %s; the delivery path was blocked", took)
	}
	if x.Stats().Dropped == 0 {
		t.Fatal("nothing was dropped against a collector that never answers; the queue is unbounded")
	}
	// Not closed: Close is bounded, but waiting it out here would only slow
	// the suite. The goroutine dies with the test server.
}

func TestARefusingCollectorIsCountedNotRetried(t *testing.T) {
	c := newCollector(t, 503)
	x, _ := NewExporter(Config{Endpoint: c.srv.URL, SampleRatio: 1, FlushEvery: 5 * time.Millisecond, Log: quiet()})
	for i := 0; i < 10; i++ {
		x.Observe("s", rawEnvelope(fmt.Sprint("E", i), "", "s", `{}`), int64(i))
	}
	until(t, "the batch to fail", func() bool { return x.Stats().Failed == 10 })
	x.Close()
	if s := x.Stats(); s.Exported != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestCloseFlushesWhatIsQueued(t *testing.T) {
	c := newCollector(t, 200)
	x, _ := NewExporter(Config{Endpoint: c.srv.URL, SampleRatio: 1, FlushEvery: time.Hour, Log: quiet()})
	for i := 0; i < 25; i++ {
		x.Observe("s", rawEnvelope(fmt.Sprint("E", i), "", "s", `{}`), int64(i))
	}
	x.Close()
	if c.count() != 25 {
		t.Fatalf("Close left %d of 25 spans unsent", 25-c.count())
	}
}

func TestExporterRefusesANonHTTPEndpoint(t *testing.T) {
	for _, bad := range []string{"", "  ", "collector:4318", "grpc://collector:4317"} {
		if _, err := NewExporter(Config{Endpoint: bad}); err == nil {
			t.Errorf("endpoint %q was accepted", bad)
		}
	}
}

func TestANilExporterIsInert(t *testing.T) {
	var x *Exporter
	x.Observe("s", []byte(`{}`), 1)
	x.Close()
	if x.Stats() != (Stats{}) {
		t.Fatal("a nil exporter reported activity")
	}
}

func TestParseHeaders(t *testing.T) {
	got := ParseHeaders(" x-api-key = abc , authorization=Bearer t=1,, broken ,=nokey")
	if len(got) != 2 || got["x-api-key"] != "abc" || got["authorization"] != "Bearer t=1" {
		t.Fatalf("parsed %v", got)
	}
}

func TestBoundedMapEvictsOldestFirst(t *testing.T) {
	b := newBoundedMap(3)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		b.put(k, k)
	}
	if _, ok := b.get("a"); ok {
		t.Fatal("a survived")
	}
	if _, ok := b.get("b"); ok {
		t.Fatal("b survived")
	}
	for _, k := range []string{"c", "d", "e"} {
		if v, ok := b.get(k); !ok || v != k {
			t.Fatalf("%s was lost", k)
		}
	}
	if len(b.m) != 3 {
		t.Fatalf("map holds %d entries at a bound of 3", len(b.m))
	}
}
