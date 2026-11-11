package executor

// Trace context and token metering — the two things a node needs to stay
// legible when it is doing a great deal at once.
//
// Neither changes what is routed or sealed. Both are bookkeeping read by the
// operator's tooling: the first joins this node's causal log to a trace the
// caller already has, the second replaces a bytes/4 guess with what the
// engines actually counted.

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"aura/kernel/internal/channel"
)

// maxTraceRoots bounds how many chains a session remembers a trace context
// for. Same shape and reasoning as the causal index: a voice session that
// lives for a day must not grow the node's memory.
const maxTraceRoots = 1024

// traceIndex maps a chain's root envelope to the `traceparent` its client
// supplied.
type traceIndex struct {
	mu    sync.Mutex
	byID  map[string]string
	order []string
	max   int
}

func newTraceIndex(max int) *traceIndex {
	return &traceIndex{byID: map[string]string{}, max: max}
}

func (t *traceIndex) set(root, trace string) {
	if t == nil || root == "" || !channel.ValidTraceparent(trace) {
		// A malformed context is dropped, not refused. Telemetry must never be
		// the reason a delivery fails: the caller asked for work, and a typo in
		// a tracing header is not a reason to deny it.
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byID[root]; !ok {
		t.order = append(t.order, root)
		if len(t.order) > t.max {
			delete(t.byID, t.order[0])
			t.order = t.order[1:]
		}
	}
	t.byID[root] = trace
}

func (t *traceIndex) get(root string) string {
	if t == nil || root == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[root]
}

// traceFor is the trace context an envelope forwarded from `src` carries.
func (s *Session) traceFor(src channel.Envelope) string {
	if trace := s.traces.get(s.rootOf(src)); trace != "" {
		return trace
	}
	// A chain whose root this index no longer holds (evicted, or a session
	// resumed from the log) still propagates whatever the source itself
	// carried, which is how a current SDK echoes it.
	if channel.ValidTraceparent(src.Trace) {
		return src.Trace
	}
	return ""
}

// TokenUsage is what skills reported their inferences cost, summed.
//
// Reported, not estimated: every number here came out of an engine's own
// counter by way of a C5 attestation. A skill that attests no usage adds
// nothing, so on a node where some skills report and some do not this is a
// floor, and the metric's help text says so.
type TokenUsage struct {
	// Input is prompt tokens, cached or not.
	Input uint64 `json:"input_tokens"`
	// Output is generated tokens.
	Output uint64 `json:"output_tokens"`
	// CachedInput is the part of Input an engine served from a prefix cache
	// instead of evaluating. Input minus this is what was actually computed —
	// the ratio is the number that says whether prompts are cache-shaped.
	CachedInput uint64 `json:"cached_input_tokens"`
	// Inferences is how many attestations carried usage at all.
	Inferences uint64 `json:"inferences"`
}

type usageCounters struct {
	input, output, cached, inferences atomic.Uint64
}

// TokenUsage snapshots the node-wide meter.
func (m *Manager) TokenUsage() TokenUsage {
	return TokenUsage{
		Input:       m.usage.input.Load(),
		Output:      m.usage.output.Load(),
		CachedInput: m.usage.cached.Load(),
		Inferences:  m.usage.inferences.Load(),
	}
}

// meterUsage adds an attestation's `usage` block (C5 v1.2) to the node's
// meter. Called once per attested envelope, after de-duplication, so an
// at-least-once redelivery is not counted twice.
func (s *Session) meterUsage(attest json.RawMessage) {
	if s.usage == nil || len(attest) == 0 {
		return
	}
	var probe struct {
		Usage *struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
			Cached *int64 `json:"cached_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(attest, &probe) != nil || probe.Usage == nil {
		return
	}
	add := func(dst *atomic.Uint64, v *int64) bool {
		if v == nil || *v < 0 {
			return false
		}
		dst.Add(uint64(*v))
		return true
	}
	in := add(&s.usage.input, probe.Usage.Input)
	out := add(&s.usage.output, probe.Usage.Output)
	add(&s.usage.cached, probe.Usage.Cached)
	if in || out {
		s.usage.inferences.Add(1)
	}
}

// acquireReplicas records this session's load on each skill connection it was
// wired to, so the next session's resolution can prefer a quieter replica.
func (s *Session) acquireReplicas() {
	for _, live := range s.replicas {
		live.Acquire()
	}
}

// releaseReplicas gives that load back. Idempotent: a session can be ended by
// its socket closing and by the ingress reaper, and must only count once.
func (s *Session) releaseReplicas() {
	s.replicaOnce.Do(func() {
		for _, live := range s.replicas {
			live.Release()
		}
	})
}
