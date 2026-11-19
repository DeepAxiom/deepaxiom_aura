package infer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"time"
)

// Relaying a response while reading two things off it: when the first token
// arrived, and what the engine says the request cost.
//
// The bytes are forwarded as they come and are not changed, with one
// exception that exists to keep the relay invisible: when the node asked the
// engine for usage on its own account, the extra chunk that produces is taken
// back out. A caller that did not ask for a usage chunk does not get one for
// having gone through the node.

// Usage is what an engine counted for one request. Reported is false when it
// said nothing — which is different from zero, and is never filled in with an
// estimate: a number this node made up would be worse than none, since
// budgets get set from it.
type Usage struct {
	Reported bool  `json:"reported"`
	Input    int64 `json:"input_tokens"`
	Output   int64 `json:"output_tokens"`
	// Cached is the part of Input the engine served from a prefix cache
	// instead of evaluating.
	Cached int64 `json:"cached_input_tokens"`
}

// observation is what the relay saw of one response.
type observation struct {
	firstToken time.Time // zero when no content ever arrived
	usage      Usage
	events     int
	bytes      int64
	// finished is true when the engine closed the stream itself — a `[DONE]`
	// or a finish_reason — rather than the stream simply ending.
	finished bool
}

// wireUsage is the OpenAI-shaped usage object, as far as this node reads it.
type wireUsage struct {
	Prompt     *int64 `json:"prompt_tokens"`
	Completion *int64 `json:"completion_tokens"`
	Details    *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// wireTimings is llama.cpp's own accounting, which it sends where other
// engines send `usage` — and sometimes instead of it.
type wireTimings struct {
	CacheN     *int64 `json:"cache_n"`
	PromptN    *int64 `json:"prompt_n"`
	PredictedN *int64 `json:"predicted_n"`
}

func (u *Usage) take(w *wireUsage) {
	if w == nil || (w.Prompt == nil && w.Completion == nil) {
		return
	}
	u.Reported = true
	if w.Prompt != nil && *w.Prompt >= 0 {
		u.Input = *w.Prompt
	}
	if w.Completion != nil && *w.Completion >= 0 {
		u.Output = *w.Completion
	}
	if w.Details != nil && w.Details.Cached != nil && *w.Details.Cached >= 0 {
		u.Cached = *w.Details.Cached
	}
}

// takeTimings fills usage from llama.cpp's `timings` when the engine sent no
// `usage` at all. `prompt_n` there is the tokens it actually evaluated and
// `cache_n` the ones it reused, so the prompt is their sum.
func (u *Usage) takeTimings(t *wireTimings) {
	if u.Reported || t == nil || (t.PromptN == nil && t.PredictedN == nil) {
		return
	}
	u.Reported = true
	if t.PromptN != nil && *t.PromptN >= 0 {
		u.Input = *t.PromptN
	}
	if t.CacheN != nil && *t.CacheN >= 0 {
		u.Cached = *t.CacheN
		u.Input += *t.CacheN
	}
	if t.PredictedN != nil && *t.PredictedN >= 0 {
		u.Output = *t.PredictedN
	}
}

// sseEvent is one streamed chunk, as far as this node reads it. Deliberately
// shallow: which fields carry text differs by engine and by endpoint, so the
// question asked is only "is there anything here a model produced".
type sseEvent struct {
	Choices []struct {
		Delta        json.RawMessage `json:"delta"`
		Text         *string         `json:"text"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
	Timings *wireTimings `json:"timings"`
}

// carriesOutput reports whether a chunk holds something the model generated:
// content, reasoning, or a tool call. A chunk that only announces a role does
// not, and neither does one that only reports usage.
func (e *sseEvent) carriesOutput() bool {
	for _, c := range e.Choices {
		if c.Text != nil && *c.Text != "" {
			return true
		}
		if len(c.Delta) == 0 {
			continue
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(c.Delta, &delta) != nil {
			continue
		}
		for key, val := range delta {
			if key == "role" {
				continue
			}
			if s := string(bytes.TrimSpace(val)); s != "" && s != "null" && s != `""` && s != "[]" {
				return true
			}
		}
	}
	return false
}

// maxInspectedLine bounds how long a line is parsed. A longer one is still
// forwarded — every byte of it — and simply not looked into.
const maxInspectedLine = 1 << 20

var (
	dataPrefix = []byte("data:")
	doneMarker = []byte("[DONE]")
)

// relaySSE copies a server-sent-event stream from src to dst, flushing at the
// end of every event so a token is on the wire as soon as the engine sent it.
//
// swallowUsageOnly drops the chunk that carries usage and no choices — the one
// that exists only because the node asked for it.
func relaySSE(dst io.Writer, flush func(), src io.Reader, obs *observation,
	swallowUsageOnly bool, now func() time.Time) error {

	r := bufio.NewReaderSize(src, 32<<10)
	skipBlank := false
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			obs.bytes += int64(len(line))
			blank := len(bytes.TrimRight(line, "\r\n")) == 0
			drop := false
			switch {
			case blank:
				// The separator after a chunk that was taken out goes with it.
				drop, skipBlank = skipBlank, false
			case bytes.HasPrefix(line, dataPrefix) && len(line) <= maxInspectedLine:
				drop = obs.inspect(bytes.TrimSpace(line[len(dataPrefix):]), swallowUsageOnly, now)
				skipBlank = drop
			default:
				skipBlank = false
			}
			if !drop {
				if _, werr := dst.Write(line); werr != nil {
					return werr
				}
				if blank {
					flush()
				}
			}
		}
		if err != nil {
			flush()
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// inspect reads one `data:` payload and reports whether to drop the line.
func (o *observation) inspect(payload []byte, swallowUsageOnly bool, now func() time.Time) bool {
	if bytes.Equal(payload, doneMarker) {
		o.finished = true
		return false
	}
	// Once the first token has been seen, the only chunks worth parsing are
	// the ones that carry accounting — and there is one of those per reply,
	// against hundreds that carry a token. Everything else is counted and
	// forwarded without being decoded, which is most of what a stream is.
	if !o.firstToken.IsZero() && !carriesAccounting(payload) {
		o.events++
		return false
	}
	var ev sseEvent
	if json.Unmarshal(payload, &ev) != nil {
		return false // not ours to understand; forwarded as it is
	}
	o.events++
	o.usage.take(ev.Usage)
	o.usage.takeTimings(ev.Timings)
	if o.firstToken.IsZero() && ev.carriesOutput() {
		o.firstToken = now()
	}
	for _, c := range ev.Choices {
		if c.FinishReason != nil {
			o.finished = true
		}
	}
	return swallowUsageOnly && ev.Usage != nil && len(ev.Choices) == 0
}

var (
	usageKey   = []byte(`"usage"`)
	timingsKey = []byte(`"timings"`)
)

// carriesAccounting reports whether a chunk has a `usage` or `timings` member
// whose value is an object. Deliberately a scan and not a parse: its whole
// purpose is to decide whether parsing is worth it.
//
// It errs on one side only. A chunk it says yes to is parsed properly, so a
// false yes costs time and nothing else. It cannot say no to real accounting
// short of an engine spelling the member in a way JSON does not allow: the
// key is matched with its quotes, so the same word inside a string — where
// its quotes would be escaped — is not mistaken for it, and `"usage":null`,
// which some engines put on every chunk, is correctly not accounting.
func carriesAccounting(payload []byte) bool {
	return hasObjectMember(payload, usageKey) || hasObjectMember(payload, timingsKey)
}

func hasObjectMember(payload, key []byte) bool {
	for at := 0; ; {
		i := bytes.Index(payload[at:], key)
		if i < 0 {
			return false
		}
		at += i + len(key)
		j := skipSpace(payload, at)
		if j < len(payload) && payload[j] == ':' {
			j = skipSpace(payload, j+1)
			if j < len(payload) && payload[j] == '{' {
				return true
			}
		}
	}
}

// skipSpace returns the index of the first byte at or after i that is not
// JSON whitespace.
func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return i
}

// maxCapturedBody bounds how much of a non-streamed response is kept to read
// usage from. The response itself is forwarded whole whatever its size.
const maxCapturedBody = 4 << 20

// relayJSON copies a non-streamed response and reads its usage.
func relayJSON(dst io.Writer, src io.Reader, obs *observation, now func() time.Time) error {
	var captured bytes.Buffer
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			obs.bytes += int64(n)
			if captured.Len() < maxCapturedBody {
				room := maxCapturedBody - captured.Len()
				if n < room {
					room = n
				}
				captured.Write(buf[:room])
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
	}
	if obs.bytes > maxCapturedBody {
		return nil // truncated copy: nothing in it can be trusted to parse
	}
	var whole struct {
		Usage   *wireUsage      `json:"usage"`
		Timings *wireTimings    `json:"timings"`
		Choices json.RawMessage `json:"choices"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(captured.Bytes(), &whole) != nil {
		return nil
	}
	obs.usage.take(whole.Usage)
	obs.usage.takeTimings(whole.Timings)
	if len(whole.Choices) > 0 || len(whole.Data) > 0 {
		// The whole answer arrived at once, so "first token" is "the answer".
		obs.firstToken = now()
		obs.finished = true
	}
	return nil
}
