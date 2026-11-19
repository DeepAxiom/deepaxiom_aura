package infer

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A stream as an OpenAI-compatible engine sends one: a role-only opener, two
// content chunks, a finish, a usage-only chunk, and the terminator.
const (
	evRole   = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	evHello  = `data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n"
	evLo     = `data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n"
	evFinish = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	evUsage  = `data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":100}}}` + "\n\n"
	evDone   = "data: [DONE]\n\n"
)

// ticker is a clock that advances a millisecond every time it is read, so the
// order in which the relay looked at things is visible in the times it kept.
func ticker() func() time.Time {
	t := time.UnixMilli(1_800_000_000_000)
	return func() time.Time {
		t = t.Add(time.Millisecond)
		return t
	}
}

func relay(t *testing.T, stream string, swallow bool) (string, observation, int) {
	t.Helper()
	var out bytes.Buffer
	var obs observation
	flushes := 0
	if err := relaySSE(&out, func() { flushes++ }, strings.NewReader(stream), &obs, swallow, ticker()); err != nil {
		t.Fatalf("relaySSE: %v", err)
	}
	return out.String(), obs, flushes
}

func TestAStreamIsRelayedByteForByte(t *testing.T) {
	stream := evRole + evHello + evLo + evFinish + evUsage + evDone
	got, obs, flushes := relay(t, stream, false)
	if got != stream {
		t.Fatalf("the relayed stream differs from the engine's:\n got %q\nwant %q", got, stream)
	}
	if !obs.finished {
		t.Fatal("a stream the engine closed itself was not seen as finished")
	}
	if !obs.usage.Reported || obs.usage.Input != 120 || obs.usage.Output != 2 || obs.usage.Cached != 100 {
		t.Fatalf("usage read as %+v", obs.usage)
	}
	// One flush per event at least: a token must be on the wire when the
	// engine sent it, not when a buffer happened to fill.
	if flushes < 6 {
		t.Fatalf("flushed %d times for 6 events", flushes)
	}
}

// The first token is the first thing a model produced. A chunk that only
// announces the role is not it, and counting it would make every engine look
// faster than it is by exactly its time to first token.
func TestTheFirstTokenIsTheFirstOutputNotTheFirstChunk(t *testing.T) {
	var out bytes.Buffer
	var obs observation
	now := ticker()
	start := now()
	if err := relaySSE(&out, func() {}, strings.NewReader(evRole+evHello+evLo+evDone), &obs, false, now); err != nil {
		t.Fatal(err)
	}
	if obs.firstToken.IsZero() {
		t.Fatal("no first token recorded")
	}
	// The clock ticks once per chunk inspected for output; the role-only chunk
	// must not have been the one that stopped it.
	if got := obs.firstToken.Sub(start); got != time.Millisecond {
		t.Fatalf("first token recorded %v after the start, want exactly one tick: "+
			"the role-only chunk must not read the clock", got)
	}
}

func TestOutputIsRecognisedWhereverAnEnginePutsIt(t *testing.T) {
	for name, ev := range map[string]string{
		"content":      `data: {"choices":[{"delta":{"content":"x"}}]}`,
		"reasoning":    `data: {"choices":[{"delta":{"reasoning_content":"x"}}]}`,
		"tool call":    `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"f"}}]}}]}`,
		"completions":  `data: {"choices":[{"text":"x"}]}`,
		"other fields": `data: {"choices":[{"delta":{"reasoning":"x"}}]}`,
	} {
		_, obs, _ := relay(t, ev+"\n\n", false)
		if obs.firstToken.IsZero() {
			t.Fatalf("%s: output was not recognised as output", name)
		}
	}
	for name, ev := range map[string]string{
		"role only":     `data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		"empty content": `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		"null content":  `data: {"choices":[{"delta":{"content":null}}]}`,
		"empty delta":   `data: {"choices":[{"delta":{}}]}`,
		"usage only":    `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		"empty tools":   `data: {"choices":[{"delta":{"tool_calls":[]}}]}`,
	} {
		_, obs, _ := relay(t, ev+"\n\n", false)
		if !obs.firstToken.IsZero() {
			t.Fatalf("%s: counted as the first token", name)
		}
	}
}

// The usage chunk exists because the node asked for it. A caller that did not
// ask must not be able to tell it went through the node.
func TestAUsageChunkTheNodeAskedForIsTakenBackOut(t *testing.T) {
	stream := evRole + evHello + evLo + evFinish + evUsage + evDone
	got, obs, _ := relay(t, stream, true)
	want := evRole + evHello + evLo + evFinish + evDone
	if got != want {
		t.Fatalf("the caller's stream:\n got %q\nwant %q", got, want)
	}
	if !obs.usage.Reported || obs.usage.Input != 120 {
		t.Fatalf("the usage was taken out of the stream and also lost: %+v", obs.usage)
	}
}

// vLLM can put `usage` on every chunk. Only the chunk with no choices is the
// extra one; the others are the caller's tokens.
func TestOnlyTheChunkWithNoChoicesIsTakenOut(t *testing.T) {
	withUsage := `data: {"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}` + "\n\n"
	got, _, _ := relay(t, withUsage+evUsage+evDone, true)
	if got != withUsage+evDone {
		t.Fatalf("got %q", got)
	}
}

// llama.cpp accounts in `timings`, and some builds send that instead of usage.
func TestLlamaCppTimingsAreReadWhenThereIsNoUsage(t *testing.T) {
	ev := `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"timings":{"cache_n":236,"prompt_n":14,"predicted_n":40}}` + "\n\n"
	_, obs, _ := relay(t, ev, false)
	if !obs.usage.Reported || obs.usage.Input != 250 || obs.usage.Cached != 236 || obs.usage.Output != 40 {
		t.Fatalf("usage from timings: %+v, want input 250 (14 evaluated + 236 cached), cached 236, output 40", obs.usage)
	}
	// And usage, where both are present, is what counts.
	both := `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3},"timings":{"cache_n":1,"prompt_n":1,"predicted_n":1}}` + "\n\n"
	_, obs, _ = relay(t, both, false)
	if obs.usage.Input != 9 || obs.usage.Output != 3 {
		t.Fatalf("timings overrode usage: %+v", obs.usage)
	}
}

func TestAnEngineThatReportsNothingLeavesUsageUnreported(t *testing.T) {
	_, obs, _ := relay(t, evRole+evHello+evDone, false)
	if obs.usage.Reported {
		t.Fatalf("usage invented for an engine that sent none: %+v", obs.usage)
	}
}

func TestWhatTheRelayDoesNotUnderstandIsForwardedUntouched(t *testing.T) {
	stream := ": keep-alive\n\n" + "event: ping\ndata: not json at all\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\r\n\r\n" + // CRLF framing
		"data: " + strings.Repeat("x", maxInspectedLine+10) + "\n\n" + // too long to parse
		"no trailing newline"
	got, _, _ := relay(t, stream, true)
	if got != stream {
		t.Fatalf("an unusual stream was altered (lengths %d vs %d)", len(got), len(stream))
	}
}

func TestANonStreamedResponseIsRelayedAndItsUsageRead(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":30,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":12}}}`
	var out bytes.Buffer
	var obs observation
	if err := relayJSON(&out, strings.NewReader(body), &obs, ticker()); err != nil {
		t.Fatal(err)
	}
	if out.String() != body {
		t.Fatal("a non-streamed response was altered")
	}
	if obs.usage.Input != 30 || obs.usage.Output != 4 || obs.usage.Cached != 12 || !obs.finished {
		t.Fatalf("obs: %+v", obs)
	}
	// An embeddings response has no completion tokens and no choices.
	emb := `{"data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":8,"total_tokens":8}}`
	out.Reset()
	obs = observation{}
	if err := relayJSON(&out, strings.NewReader(emb), &obs, ticker()); err != nil {
		t.Fatal(err)
	}
	if !obs.usage.Reported || obs.usage.Input != 8 || obs.usage.Output != 0 {
		t.Fatalf("embeddings usage: %+v", obs.usage)
	}
}

// The fast path decides, without parsing, whether a chunk is worth parsing.
// It may say yes too often; it must never say no to real accounting.
func TestAccountingIsRecognisedWithoutParsing(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"choices":[],"usage":{"prompt_tokens":1}}`:                    true,
		`{"choices":[], "usage" : { "prompt_tokens": 1 }}`:              true,
		"{\"usage\"\n:\t{\"prompt_tokens\":1}}":                         true,
		`{"choices":[{"delta":{}}],"timings":{"cache_n":3}}`:            true,
		`{"choices":[{"delta":{"content":"x"}}],"usage":null}`:          false,
		`{"choices":[{"delta":{"content":"x"}}], "usage": null}`:        false,
		`{"choices":[{"delta":{"content":"x"}}]}`:                       false,
		`{"choices":[{"delta":{"content":"the \"usage\": {} field"}}]}`: false,
		`{"choices":[{"delta":{"content":"usage: {"}}]}`:                false,
		`{"usage":7}`: false,
		`{"usage":null,"timings":{"predicted_n":4}}`: true,
	} {
		if got := carriesAccounting([]byte(raw)); got != want {
			t.Fatalf("carriesAccounting(%s) = %v, want %v", raw, got, want)
		}
	}
}

// A long reply with usage spelled the way vLLM spells it — null on every
// token, an object at the end — is relayed whole, its usage read, and the
// extra chunk taken out, whether or not each line was parsed.
func TestALongStreamIsRelayedWholeAndItsUsageRead(t *testing.T) {
	token := `data: {"choices":[{"index":0,"delta":{"content":"tok"},"finish_reason":null}],"usage":null}` + "\n\n"
	var stream strings.Builder
	stream.WriteString(evRole)
	for i := 0; i < 400; i++ {
		stream.WriteString(token)
	}
	stream.WriteString(evFinish + evUsage + evDone)

	got, obs, _ := relay(t, stream.String(), true)
	want := strings.Replace(stream.String(), evUsage, "", 1)
	if got != want {
		t.Fatalf("a long stream was altered (lengths %d vs %d)", len(got), len(want))
	}
	if obs.usage.Input != 120 || obs.usage.Output != 2 || obs.usage.Cached != 100 {
		t.Fatalf("usage %+v", obs.usage)
	}
	if obs.events != 403 {
		t.Fatalf("counted %d events, want 403", obs.events)
	}
	if !obs.finished {
		t.Fatal("the stream's end was not seen")
	}
}
