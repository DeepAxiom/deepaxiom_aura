package infer

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// What the plane costs per streamed token and per request, in isolation: no
// network, no engine. The end-to-end figure, sockets included, comes from
// scripts/inference_e2e.py.
//
//	go test ./internal/infer/ -run xxx -bench . -benchmem

// A realistic chunk: what vLLM and llama.cpp put on the wire for one token.
const benchChunk = `data: {"id":"chatcmpl-9f2c","object":"chat.completion.chunk","created":1800000000,` +
	`"model":"qwen3-8b","choices":[{"index":0,"delta":{"content":" token"},"logprobs":null,"finish_reason":null}]}` + "\n\n"

func benchStream(events int) []byte {
	var b bytes.Buffer
	for i := 0; i < events; i++ {
		b.WriteString(benchChunk)
	}
	b.WriteString(evUsage + evDone)
	return b.Bytes()
}

// BenchmarkRelayPerToken is the node's own cost for one token passing through:
// read the line, look at it, write it, flush.
func BenchmarkRelayPerToken(b *testing.B) {
	const events = 512
	stream := benchStream(events)
	now := time.Now
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var obs observation
		if err := relaySSE(io.Discard, func() {}, bytes.NewReader(stream), &obs, true, now); err != nil {
			b.Fatal(err)
		}
		if !obs.usage.Reported {
			b.Fatal("usage was not read")
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/events, "ns/token")
}

// BenchmarkForwardPerToken is the floor: the same stream moved line by line
// and flushed per event, without being looked at. The difference from
// BenchmarkRelayPerToken is what reading the stream costs.
func BenchmarkForwardPerToken(b *testing.B) {
	const events = 512
	stream := benchStream(events)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := bufio.NewReaderSize(bytes.NewReader(stream), 32<<10)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				if _, werr := io.Discard.Write(line); werr != nil {
					b.Fatal(werr)
				}
			}
			if err != nil {
				break
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/events, "ns/token")
}

// BenchmarkPrepareRequest is what the node does to a request before sending
// it: map the body's top level, ask for usage, take the caller's priority out
// and write the node's.
func BenchmarkPrepareRequest(b *testing.B) {
	for _, size := range []struct {
		name  string
		turns int
	}{{"short chat", 4}, {"long chat", 200}} {
		b.Run(size.name, func(b *testing.B) {
			var messages strings.Builder
			messages.WriteString(`[{"role":"system","content":"You are a careful assistant."}`)
			for i := 0; i < size.turns; i++ {
				messages.WriteString(`,{"role":"user","content":"What changed in the last frame, and is it safe to proceed?"}`)
				messages.WriteString(`,{"role":"assistant","content":"The pallet moved 12 cm to the left. Path is clear; proceeding at reduced speed."}`)
			}
			messages.WriteString(`]`)
			raw := []byte(`{"model":"qwen3-8b","messages":` + messages.String() +
				`,"max_tokens":256,"temperature":0.2,"stream":true,"priority":5}`)
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				body, err := parseBody(raw)
				if err != nil {
					b.Fatal(err)
				}
				existing, _ := body.get("stream_options")
				body.set("stream_options", withUsage(existing))
				body.remove("priority")
				body.set("priority", []byte("-7"))
				if len(body.bytes()) == 0 {
					b.Fatal("empty body")
				}
			}
		})
	}
}
