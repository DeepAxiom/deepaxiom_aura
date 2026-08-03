package fed

import (
	"net"
	"net/url"
	"strings"
	"time"
)

// RouteClass is what a bridge decided about how it reaches a remote node —
// C3 rule 5's "the control plane picks the best route (same process → LAN →
// QUIC/WebRTC → kernel relay)" made into something `aura federate` actually
// measures and reports, not an unmeasured claim. H1 (this package) only
// distinguishes the two ends of that spectrum it can act on today —
// same-host and LAN both get the connection-pooling treatment in bridge.go;
// QUIC/WebRTC negotiation is unbuilt, so anything else is still relay, named
// honestly rather than left implicit.
type RouteClass string

const (
	RouteSameHost RouteClass = "same-host"
	RouteLAN      RouteClass = "lan"
	RouteRelay    RouteClass = "relay"
)

// lanThreshold is a coarse, deliberately generous cutoff — this classifies
// for observability and connection-reuse decisions, not for anything a
// correctness guarantee depends on, so it does not need to be precise.
const lanThreshold = 15 * time.Millisecond

// classifyRoute turns a remote URL and a measured round-trip time into a
// RouteClass. Loopback is checked before the RTT — a remote on the same
// host can occasionally measure a slow first round-trip (DNS, a cold TCP
// stack) that has nothing to do with the route actually available.
func classifyRoute(remoteURL string, rtt time.Duration) RouteClass {
	if isLoopbackHost(remoteURL) {
		return RouteSameHost
	}
	if rtt <= lanThreshold {
		return RouteLAN
	}
	return RouteRelay
}

func isLoopbackHost(remoteURL string) bool {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}
