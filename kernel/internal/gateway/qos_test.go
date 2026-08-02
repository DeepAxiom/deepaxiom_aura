package gateway

import (
	"testing"
	"time"

	"aura/kernel/internal/channel"
)

// laneWriter builds a wsWriter with no socket and no drain goroutine, so the
// lanes fill and stay full. That is exactly the condition the QoS classes
// differ under, and it needs no network to reproduce.
func laneWriter(reliable, realtime int) *wsWriter {
	return &wsWriter{
		ch:   make(chan []byte, reliable),
		rt:   make(chan []byte, realtime),
		done: make(chan struct{}),
	}
}

func drain(ch chan []byte) [][]byte {
	var out [][]byte
	for {
		select {
		case raw := <-ch:
			out = append(out, raw)
		default:
			return out
		}
	}
}

// The property the whole voice path rests on: a producer of audio is never
// stalled by a slow consumer. Blocking here would mean a skill's read loop
// stops, which stalls every other session it serves too.
func TestRealtimeSendNeverBlocksWhenTheLaneIsFull(t *testing.T) {
	w := laneWriter(4, 4)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if err := w.Send([]byte{byte(i)}, channel.QoSRealtime); err != nil {
				t.Errorf("realtime send %d returned %v; it must never fail", i, err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("realtime Send blocked on a full lane")
	}
}

// Under pressure the OLDEST frame goes, not the newest. In a live stream the
// stale frame is the worthless one — keeping it and dropping what just arrived
// would make the receiver fall further behind with every drop.
func TestRealtimeDropsTheOldestFrame(t *testing.T) {
	w := laneWriter(4, 3)

	for i := byte(1); i <= 6; i++ {
		if err := w.Send([]byte{i}, channel.QoSRealtime); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	queued := drain(w.rt)
	if len(queued) != 3 {
		t.Fatalf("lane holds %d frame(s); want its capacity of 3", len(queued))
	}
	// The three most recent survive: 4, 5, 6.
	for i, raw := range queued {
		if want := byte(4 + i); raw[0] != want {
			t.Fatalf("queued[%d] = %d, want %d — the newest frames must survive", i, raw[0], want)
		}
	}
}

// The reliable lane keeps its old behaviour: it fills up and then makes the
// producer wait, because losing a message there is not an option.
func TestReliableSendBlocksAndThenReportsBackpressure(t *testing.T) {
	old := backpressureTimeout
	backpressureTimeout = 150 * time.Millisecond
	t.Cleanup(func() { backpressureTimeout = old })

	w := laneWriter(2, 4)

	for i := 0; i < 2; i++ {
		if err := w.Send([]byte{byte(i)}, channel.QoSReliable); err != nil {
			t.Fatalf("send %d should fit: %v", i, err)
		}
	}

	start := time.Now()
	err := w.Send([]byte{9}, channel.QoSReliable)
	if err == nil {
		t.Fatal("want an error once the reliable lane is full and nobody drains it")
	}
	if elapsed := time.Since(start); elapsed < backpressureTimeout {
		t.Fatalf("returned after %v; it should wait for the receiver first", elapsed)
	}
	if n := len(drain(w.ch)); n != 2 {
		t.Fatalf("reliable lane holds %d; it must not drop to make room", n)
	}
}

// `bulk` is named by C3 but its behaviour is not defined. Treating it as
// reliable is the safe reading: it never silently loses anything.
func TestUnknownQoSIsTreatedAsReliable(t *testing.T) {
	w := laneWriter(2, 2)

	if err := w.Send([]byte{1}, channel.QoSBulk); err != nil {
		t.Fatalf("bulk send: %v", err)
	}
	if err := w.Send([]byte{2}, ""); err != nil {
		t.Fatalf("empty qos should default to reliable: %v", err)
	}
	if n := len(drain(w.ch)); n != 2 {
		t.Fatalf("reliable lane got %d frame(s), want 2", n)
	}
	if n := len(drain(w.rt)); n != 0 {
		t.Fatalf("realtime lane got %d frame(s), want 0", n)
	}
}

// A closed connection must be reported even on the lossy lane: "dropped a
// frame" and "there is no peer any more" are different things to the caller.
func TestRealtimeSendReportsAClosedConnection(t *testing.T) {
	w := laneWriter(2, 0)
	close(w.done)

	if err := w.Send([]byte{1}, channel.QoSRealtime); err == nil {
		t.Fatal("want an error once the connection is closed")
	}
}
