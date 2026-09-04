package queue

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 10 * time.Second},  // a nonsensical attempt count still waits
		{1, 10 * time.Second},  // first retry
		{2, 20 * time.Second},  // doubling
		{3, 40 * time.Second},  // still doubling
		{7, 10 * time.Minute},  // the cap, reached
		{99, 10 * time.Minute}, // and never exceeded
	}
	for _, c := range cases {
		if got := Backoff(c.attempt); got != c.want {
			t.Errorf("Backoff(%d) = %s, want %s", c.attempt, got, c.want)
		}
	}
	// Monotonic up to the cap: a retry must never be scheduled sooner than the
	// one before it, which is what turns a permanently broken source into a
	// worker that does nothing else.
	prev := time.Duration(0)
	for attempt := 1; attempt <= 20; attempt++ {
		got := Backoff(attempt)
		if got < prev {
			t.Fatalf("Backoff(%d) = %s went backwards from %s", attempt, got, prev)
		}
		prev = got
	}
}
