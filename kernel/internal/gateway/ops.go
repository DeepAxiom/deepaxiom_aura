package gateway

// Operational endpoints — the two an orchestrator needs and this node did not
// have.
//
// `/healthz` already existed and answers "is this process alive and what is it".
// Neither of the questions a scheduler actually asks was answerable:
//
//   - **Readiness.** A liveness probe that doubles as a readiness probe sends
//     traffic to a node whose store opened but whose saved projections have not
//     reconnected, or whose ledger refused to open. Under a rolling deploy that
//     means the new pod takes traffic and fails it while the old one is being
//     torn down — the classic reason a "zero-downtime" deploy is not one.
//   - **Metrics.** The write path is instrumented (group-commit batch sizes,
//     fallbacks, mean commit time) and the event log reports its own size and
//     damage. All of it was computed and then printed once, at startup, when
//     every counter was zero. An operator asking "is the write path my ceiling?"
//     had no way to find out on a running node.
//
// Prometheus text format, emitted by hand. This project has six direct
// dependencies and a client library for a text format with four line shapes is
// not worth being the seventh — the exposition format is stable and documented,
// and writing it out keeps the metric names in the same file as the reason each
// one exists.

import (
	"fmt"
	"net/http"
	"strings"
)

// readiness reports whether this node should be sent traffic.
//
// Deliberately stricter than health. A node answers `/healthz` as soon as its
// HTTP listener is up; it answers `/readyz` only once the parts a request will
// actually touch are usable. The distinction is what makes a rolling deploy
// safe, and conflating them is why so many are not.
func (g *Gateway) readiness(w http.ResponseWriter, r *http.Request) {
	type check struct {
		Name   string `json:"name"`
		Ready  bool   `json:"ready"`
		Detail string `json:"detail,omitempty"`
	}
	var checks []check
	ready := true

	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{Name: name, Ready: ok, Detail: detail})
		if !ok {
			ready = false
		}
	}

	// The store is the floor: without it there is no session, no event log and
	// no ledger, and a request would fail after being accepted.
	add("store", g.St != nil, "the embedded state store is open")

	// The ledger is not optional on a node that seals effects. A node running
	// without one would accept a motor delivery and be unable to attest it,
	// which is the one failure this runtime exists to prevent.
	add("ledger", g.Ldg != nil, "the effect ledger is open")

	// The registry has to exist for a capability to resolve. Zero skills
	// connected is *not* unready — a node with an empty catalogue is a correct,
	// idle node, and failing readiness on it would mean a fresh deploy never
	// becomes ready until something else connects to it.
	add("registry", g.Reg != nil, "capability resolution is available")

	// A damaged event-log segment does not stop the node, and it must not be
	// hidden either: a reader will get less history than it asked for.
	if g.St != nil {
		if d := g.St.EventLogStats().Damage; len(d) > 0 {
			checks = append(checks, check{Name: "event_log",
				Ready: true, Detail: "readable, with damage: " + strings.Join(d, "; ")})
		}
	}

	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"ready": ready, "checks": checks})
}

// metrics emits the Prometheus text exposition format.
//
// Every series here answers a question an operator asks during an incident, and
// the help text says which. A metric nobody can act on is a metric that makes a
// dashboard longer and an outage no shorter.
func (g *Gateway) metrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder

	emit := func(name, help, typ string, value float64, labels ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		if len(labels) == 0 {
			fmt.Fprintf(&b, "%s %g\n", name, value)
			return
		}
		fmt.Fprintf(&b, "%s{%s} %g\n", name, strings.Join(labels, ","), value)
	}

	emit("aura_up", "1 when the node is serving.", "gauge", 1)

	if g.Mgr != nil {
		emit("aura_sessions_live",
			"Sessions currently wired and routing. Against --max-sessions this is "+
				"how close the node is to refusing new ones.",
			"gauge", float64(g.Mgr.LiveSessions()))
	}
	if g.Reg != nil {
		emit("aura_skills_connected",
			"Skills currently registered. A drop here with no deploy is a skill that died.",
			"gauge", float64(len(g.Reg.Catalog())))
	}

	if g.St != nil {
		ws := g.St.WriteStats()
		emit("aura_store_commits_total",
			"Group-commit transactions committed.", "counter", float64(ws.Batches))
		emit("aura_store_rows_total",
			"Rows written through the batcher.", "counter", float64(ws.Rows))
		emit("aura_store_batch_mean",
			"Mean rows per commit. THE number for capacity: near maxBatch means the "+
				"commit itself is the ceiling and more writers would not help; near 1 "+
				"under load means the limit is elsewhere.",
			"gauge", ws.MeanBatch)
		emit("aura_store_batch_largest",
			"Largest batch seen.", "gauge", float64(ws.LargestBatch))
		emit("aura_store_commit_ms_mean",
			"Mean time to commit one batch.", "gauge", ws.MeanCommitMs)
		emit("aura_store_commit_fallbacks_total",
			"Batches that failed and were re-run one row at a time. Non-zero means "+
				"some write is failing a constraint; each one is a slow commit.",
			"counter", float64(ws.FallbackCount))

		el := g.St.EventLogStats()
		emit("aura_event_log_bytes",
			"Causal event log on disk. Against --event-log-max, how close retention "+
				"is to reclaiming the oldest segments.",
			"gauge", float64(el.Bytes))
		emit("aura_event_log_segments", "Live event-log segments.", "gauge", float64(el.Segments))
		emit("aura_event_log_sessions", "Sessions the index knows.", "gauge", float64(el.Sessions))
		emit("aura_event_log_indexed_locators",
			"Record locators held in memory. Bounded by configuration, not by uptime.",
			"gauge", float64(el.Indexed))
		emit("aura_event_log_evicted_sessions",
			"Sessions whose locators were evicted and which now read by scanning.",
			"gauge", float64(el.Evicted))
		emit("aura_event_log_damaged_segments",
			"Sealed segments that did not read back cleanly. Non-zero means a file "+
				"changed after it was closed — investigate, do not restart.",
			"gauge", float64(len(el.Damage)))
	}

	if g.Ldg != nil {
		if sum, err := g.Ldg.Summarize(); err == nil {
			emit("aura_ledger_entries", "Effects sealed.", "gauge", float64(sum.Entries))
			emit("aura_ledger_checkpoints",
				"Signed checkpoints. Flat while entries climb means checkpointing stopped.",
				"gauge", float64(sum.Checkpoints))
		}
	}

	if g.Approvals != nil {
		emit("aura_approvals_pending",
			"Effects held at a gate waiting for a human. A rising number is the "+
				"queue a reviewer is behind on, and every one of them is a stalled chain.",
			"gauge", float64(len(g.Approvals.List())))
	}

	if g.Mgr != nil {
		mb := g.Mgr.MailboxStats()
		if mb.Enabled {
			emit("aura_mailbox_queued",
				"Envelopes waiting in per-session dispatch queues. Near zero on a healthy "+
					"node; sustained growth is consumers slower than what feeds them.",
				"gauge", float64(mb.Queued))
			emit("aura_mailbox_waits_total",
				"Times a skill's read loop found one session's queue full and had to wait. "+
					"The number to read before raising --session-mailbox: it is one slow "+
					"session, not a small queue, if it rises while aura_mailbox_queued stays low.",
				"counter", float64(mb.Waits))
			emit("aura_mailbox_dropped_total",
				"Envelopes that arrived for a session as it ended and were discarded.",
				"counter", float64(mb.Dropped))
		}

		u := g.Mgr.TokenUsage()
		const usageHelp = "Tokens skills reported in C5 attestations, by kind. Reported, not " +
			"estimated, and therefore a floor: a skill that attests no usage adds nothing. " +
			"cached_input over input is the prefix-cache hit rate."
		fmt.Fprintf(&b, "# HELP aura_tokens_total %s\n# TYPE aura_tokens_total counter\n", usageHelp)
		fmt.Fprintf(&b, "aura_tokens_total{kind=\"input\"} %d\n", u.Input)
		fmt.Fprintf(&b, "aura_tokens_total{kind=\"output\"} %d\n", u.Output)
		fmt.Fprintf(&b, "aura_tokens_total{kind=\"cached_input\"} %d\n", u.CachedInput)
		emit("aura_inferences_metered_total",
			"Inferences whose attestation carried token usage.", "counter", float64(u.Inferences))
	}

	if g.Traces != nil {
		ts := g.Traces.Stats()
		emit("aura_trace_spans_exported_total",
			"Spans a collector acknowledged.", "counter", float64(ts.Exported))
		emit("aura_trace_spans_dropped_total",
			"Spans dropped because the export queue was full. The collector is slower than "+
				"the node; lower --otlp-sample.", "counter", float64(ts.Dropped))
		emit("aura_trace_spans_failed_total",
			"Spans in batches the collector refused or never answered. The causal log is "+
				"unaffected; `aura trace <session>` can still export any of them.",
			"counter", float64(ts.Failed))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}
