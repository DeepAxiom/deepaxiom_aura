package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"aura/kernel/internal/executor"
	"aura/kernel/internal/otel"
	"aura/kernel/internal/registry"
	"aura/kernel/internal/spec"
)

// GET /v1/stats — what the node is doing right now, as JSON.
//
// /metrics already carries most of this, in the format a scraper wants. This
// is the same picture in the format a control plane wants: one request, one
// document, structured — including the one thing a flat metric cannot carry
// well, which is *which replica of which package is holding how many
// sessions*. That is the question behind "is running a second copy of this
// actually helping", and it is what the dispatch and load-aware resolution
// added in this release are judged by.
//
// Operator-only, like the catalog and for the same reason: it names every
// effect this node can produce and how busy each is.
//
// Additive: every member may be absent on a node that does not run that part
// (no exporter, inline dispatch), and a reader must treat an absent member as
// "not in use" rather than as zero.
type nodeStats struct {
	At        int64            `json:"at"` // unix millis
	UptimeS   int64            `json:"uptime_s"`
	Version   string           `json:"version,omitempty"`
	Sessions  int              `json:"sessions_live"`
	Expertise []expertiseStats `json:"expertise"`

	Mailbox     *executor.MailboxStats     `json:"mailbox,omitempty"`
	Tokens      *executor.TokenUsage       `json:"tokens,omitempty"`
	Speculation *executor.SpeculationStats `json:"speculation,omitempty"`
	Watch       *executor.WatchStats       `json:"watch,omitempty"`
	Traces      *otel.Stats                `json:"traces,omitempty"`
	Store       *storeStats                `json:"store,omitempty"`
	EventLog    *eventLogStats             `json:"event_log,omitempty"`
	Approvals   *int                       `json:"approvals_pending,omitempty"`
}

// expertiseStats is one connected package and the load on each of its
// replicas.
type expertiseStats struct {
	ID         string `json:"id"`
	Capability string `json:"capability"`
	Type       string `json:"type"`
	// Replicas is the live-session load of each connection of this package,
	// oldest connection first.
	Replicas []int64 `json:"replicas"`
	Sessions int64   `json:"sessions"`
}

type storeStats struct {
	Commits      uint64  `json:"commits"`
	Rows         uint64  `json:"rows"`
	BatchMean    float64 `json:"batch_mean"`
	BatchLargest uint64  `json:"batch_largest"`
	CommitMsMean float64 `json:"commit_ms_mean"`
}

type eventLogStats struct {
	Bytes    int64 `json:"bytes"`
	Segments int   `json:"segments"`
	Sessions int   `json:"sessions"`
}

var processStarted = time.Now()

func (g *Gateway) stats(w http.ResponseWriter, r *http.Request) {
	out := nodeStats{
		At:        time.Now().UnixMilli(),
		UptimeS:   int64(time.Since(processStarted).Seconds()),
		Version:   spec.Version,
		Expertise: []expertiseStats{},
	}
	if g.Mgr != nil {
		out.Sessions = g.Mgr.LiveSessions()
		mb := g.Mgr.MailboxStats()
		out.Mailbox = &mb
		tokens := g.Mgr.TokenUsage()
		out.Tokens = &tokens
		spec := g.Mgr.SpeculationStats()
		out.Speculation = &spec
		watch := g.Mgr.WatchStats()
		out.Watch = &watch
	}
	if g.Reg != nil {
		out.Expertise = expertiseLoad(g.Reg)
	}
	if g.Traces != nil {
		ts := g.Traces.Stats()
		out.Traces = &ts
	}
	if g.St != nil {
		ws := g.St.WriteStats()
		out.Store = &storeStats{Commits: ws.Batches, Rows: ws.Rows, BatchMean: ws.MeanBatch,
			BatchLargest: ws.LargestBatch, CommitMsMean: ws.MeanCommitMs}
		el := g.St.EventLogStats()
		out.EventLog = &eventLogStats{Bytes: el.Bytes, Segments: el.Segments, Sessions: el.Sessions}
	}
	if g.Approvals != nil {
		n := len(g.Approvals.List())
		out.Approvals = &n
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func expertiseLoad(reg *registry.Registry) []expertiseStats {
	byID := map[string]*expertiseStats{}
	var order []string
	for _, inst := range reg.Instances() {
		e, ok := byID[inst.ID]
		if !ok {
			e = &expertiseStats{ID: inst.ID, Capability: inst.Capability, Type: inst.Type,
				Replicas: []int64{}}
			byID[inst.ID] = e
			order = append(order, inst.ID)
		}
		e.Replicas = append(e.Replicas, inst.Load)
		e.Sessions += inst.Load
	}
	out := make([]expertiseStats, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}
