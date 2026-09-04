package id

import (
	"strings"
	"testing"
)

func TestNewIsSortableAndUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	var prev string
	for i := 0; i < 1000; i++ {
		got := New()
		if len(got) != 26 {
			t.Fatalf("length %d, want 26: %q", len(got), got)
		}
		if strings.ContainsFunc(got, func(r rune) bool { return !strings.ContainsRune(alphabet, r) }) {
			t.Fatalf("id outside the alphabet: %q", got)
		}
		if seen[got] {
			t.Fatalf("collision after %d ids: %q", i, got)
		}
		seen[got] = true
		// Same millisecond or later, so the time prefix never goes backwards.
		if prev != "" && got[:10] < prev[:10] {
			t.Fatalf("time prefix went backwards: %q then %q", prev, got)
		}
		prev = got
	}
}
