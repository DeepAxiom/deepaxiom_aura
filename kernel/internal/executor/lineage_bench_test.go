package executor

import (
	"fmt"
	"testing"
)

// What delivery lineage costs on the forward path, in isolation: one record
// per delivery to an Expertise, one touch per emission.
//
//	go test ./internal/executor/ -run xxx -bench Lineage -benchmem

func BenchmarkLineageRecord(b *testing.B) {
	idx := newLineageIndex(maxLineage, nil)
	ids := make([]string, 2*maxLineage) // cycles, so most records also evict
	for i := range ids {
		ids[i] = fmt.Sprintf("01JD%022d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.record(delivery{
			id: ids[i%len(ids)], prev: ids[(i+1)%len(ids)], root: "root",
			ref: "brain", port: "text_in", expertise: "acme/cognitive/llm",
			validUntil: 1_800_000_000_000,
		})
	}
}

func BenchmarkLineageTouch(b *testing.B) {
	idx := newLineageIndex(maxLineage, nil)
	ids := make([]string, maxLineage)
	for i := range ids {
		ids[i] = fmt.Sprintf("01JD%022d", i)
		idx.record(delivery{id: ids[i], expertise: "acme/cognitive/llm"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := idx.touch(ids[i%len(ids)]); !ok {
			b.Fatal("entry missing")
		}
	}
}

// The pair a streaming reply actually does per token: the emission touches
// the delivery it answers, and the forward records the delivery it makes.
func BenchmarkLineagePerStreamedToken(b *testing.B) {
	idx := newLineageIndex(maxLineage, nil)
	idx.record(delivery{id: "answering", expertise: "acme/cognitive/llm", respondBy: 1})
	ids := make([]string, 2*maxLineage)
	for i := range ids {
		ids[i] = fmt.Sprintf("01JD%022d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cause, _ := idx.touch("answering")
		if cause.respondBy != 0 {
			idx.takeFront(cause.id)
		}
		idx.record(delivery{id: ids[i%len(ids)], prev: cause.id, expertise: "acme/motor/tts"})
	}
}
