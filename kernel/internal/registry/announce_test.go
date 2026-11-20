package registry

import (
	"sync/atomic"
	"testing"
	"time"
)

// What RegisterAnnounced promises is an order: the announcement first, and only
// then a connection anyone can find. A resolver spinning on the capability the
// whole time is the adversary — it will see the connection at the first instant
// it is visible, and at that instant the announcement must already be made.
func TestTheAnnouncementIsMadeBeforeTheConnectionCanBeResolved(t *testing.T) {
	r := New()
	live := &Live{Manifest: Manifest{ID: "acme/logical/eager", Capability: "logical.eager"}}

	var announced atomic.Bool
	found := make(chan bool, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := r.Resolve("", "logical.eager"); err == nil {
				found <- announced.Load()
				return
			}
		}
	}()

	n := r.RegisterAnnounced("conn-1", live, func() {
		// Long enough that a registry which made the connection visible first
		// would be caught many thousands of times over.
		time.Sleep(20 * time.Millisecond)
		announced.Store(true)
	})
	if n != 1 {
		t.Errorf("one connection registered, counted %d", n)
	}
	select {
	case after := <-found:
		if !after {
			t.Fatal("the connection could be resolved before its announcement had been made")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection was registered and never became resolvable")
	}
}

// Register is RegisterAnnounced with nothing to announce, and counts the same.
func TestRegisterStillCountsReplicas(t *testing.T) {
	r := New()
	m := Manifest{ID: "acme/logical/twin", Capability: "logical.twin"}
	if n := r.Register("a", &Live{Manifest: m}); n != 1 {
		t.Errorf("first replica counted as %d", n)
	}
	if n := r.RegisterAnnounced("b", &Live{Manifest: m}, func() {}); n != 2 {
		t.Errorf("second replica counted as %d", n)
	}
	r.Unregister("a")
	if n := r.Register("c", &Live{Manifest: m}); n != 2 {
		t.Errorf("after one left and one joined, counted %d", n)
	}
}
