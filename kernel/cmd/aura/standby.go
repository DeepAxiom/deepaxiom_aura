package main

import (
	"fmt"
	"net"
	"time"

	"aura/kernel/internal/lease"
)

// reportTakeover says how a standby came to be the writer. Printed because it
// is the first thing anyone asks after a failover: did it take over because
// the leader was gone, or because the leader was merely slow?
func reportTakeover(t lease.StandbyResult) {
	waited := t.Waited.Round(time.Millisecond)
	switch {
	case t.From == "":
		fmt.Printf("  the data directory was free — started as the writer after %s\n", waited)
	case t.ProcessGone:
		fmt.Printf("  TOOK OVER from %s after %s: its process no longer exists\n", t.From, waited)
	default:
		fmt.Printf("  TOOK OVER from %s after %s: its heartbeat expired\n", t.From, waited)
	}
}

// waitForPort waits until the TCP port can be bound, up to maxWait.
//
// It binds and immediately releases, so there is a moment between this
// returning and the server listening in which something else could take the
// port. That is the same exposure any process has between choosing a port and
// binding it, and the bind that follows still fails loudly if it happens.
func waitForPort(listen string, port int, maxWait time.Duration) error {
	host := listen
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	deadline := time.Now().Add(maxWait)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("took over the data directory, but %s is still in use after %s: %w",
				addr, maxWait, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
