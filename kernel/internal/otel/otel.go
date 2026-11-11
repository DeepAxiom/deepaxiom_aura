// Package otel turns the causal event log into OpenTelemetry spans.
//
// The log is already a trace: every envelope names the envelope that caused
// it, so one span per envelope with parent = cause renders the causal tree in
// any OTLP backend. This package holds that mapping in one place, used twice —
// by `aura trace`, which exports a finished session after the fact, and by
// the live Exporter, which ships spans while the node runs.
//
// # Joining someone else's trace
//
// Until C3 v1.7 a session's trace id was derived from the session id, so the
// node's spans were a tree of their own that an operator had to line up with
// their application's trace by timestamp. An envelope may now carry a W3C
// `traceparent`; when the chain it belongs to has one, its spans take that
// trace id and the chain's first span hangs off the caller's span. The
// request that started in someone's backend and the effect this node sealed
// are then one trace.
//
// A chain with no context keeps the derived id, exactly as before.
//
// # What is exported about a model
//
// Where an envelope carries a C5 attestation, its span gets the GenAI
// semantic-convention attributes — the model, the engine, and the token usage
// the engine reported. That is what lets a tracing backend answer "which
// replies were slow and how many tokens did they evaluate" without this node
// growing a metrics pipeline of its own.
package otel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"aura/kernel/internal/channel"
)

// SpanID is the span id of an envelope: stable, so the batch export and the
// live one agree and a re-export does not duplicate a tree.
func SpanID(envelopeID string) string {
	sum := sha256.Sum256([]byte("span:" + envelopeID))
	return hex.EncodeToString(sum[:8])
}

// TraceID is the trace id of a session that has no trace context of its own.
func TraceID(session string) string {
	sum := sha256.Sum256([]byte("trace:" + session))
	return hex.EncodeToString(sum[:16])
}

// traceparentIDs splits a validated traceparent into its trace and parent ids.
func traceparentIDs(tp string) (traceID, parentID string, ok bool) {
	if !channel.ValidTraceparent(tp) {
		return "", "", false
	}
	return tp[3:35], tp[36:52], true
}

// SpanInput is everything one span needs.
type SpanInput struct {
	Session  string
	Envelope channel.Envelope
	// TSMillis is when the envelope was persisted.
	TSMillis int64
	// ParentKnown is whether the envelope's cause is a span in the same trace.
	// A span is only given a parent the backend will actually receive.
	ParentKnown bool
	// Trace is the chain's trace context, or "" for the derived trace id.
	Trace string
	// Payload, when non-empty, is attached as `aura.payload`. The live
	// exporter never sets it: payloads are user content, and a telemetry
	// backend is one more place for it to be retained that nobody chose.
	Payload string
	// ErrorMessage is attached to the span status of an `error` envelope.
	ErrorMessage string
	// WithSession adds an `aura.session` attribute, for a document that mixes
	// several sessions under one resource.
	WithSession bool
}

// Span builds the OTLP/JSON span for one envelope.
func Span(in SpanInput) map[string]any {
	e := in.Envelope
	ts := in.TSMillis
	if ts <= 0 {
		ts = time.Now().UnixMilli()
	}
	startNano := ts * int64(time.Millisecond)

	name := e.Kind
	if e.Node != "" {
		name = e.Node
		if e.Port != "" {
			name += "." + e.Port
		}
		name += " " + e.Kind
	}

	tid := TraceID(in.Session)
	callerSpan := ""
	if t, parent, ok := traceparentIDs(in.Trace); ok {
		tid, callerSpan = t, parent
	}

	attrs := []map[string]any{
		StrAttr("aura.kind", e.Kind),
		StrAttr("aura.node", e.Node),
		StrAttr("aura.port", e.Port),
		StrAttr("aura.schema", e.Schema),
		StrAttr("aura.envelope_id", e.ID),
	}
	if in.Payload != "" {
		attrs = append(attrs, StrAttr("aura.payload", in.Payload))
	}
	if in.WithSession {
		attrs = append(attrs, StrAttr("aura.session", in.Session))
	}
	if e.Receipt != "" {
		// The join between a span and the effect ledger: the hash `aura
		// receipt` and `aura verify` take.
		attrs = append(attrs, StrAttr("aura.receipt", e.Receipt))
	}
	attrs = append(attrs, genAIAttrs(e.Attest)...)

	span := map[string]any{
		"traceId":           tid,
		"spanId":            SpanID(e.ID),
		"name":              name,
		"kind":              1, // SPAN_KIND_INTERNAL
		"startTimeUnixNano": fmt.Sprint(startNano),
		"endTimeUnixNano":   fmt.Sprint(startNano + int64(time.Millisecond)),
		"attributes":        attrs,
	}
	switch {
	case e.CauseID != "" && in.ParentKnown:
		span["parentSpanId"] = SpanID(e.CauseID)
	case callerSpan != "":
		// The first span of a chain the caller started: it hangs off theirs.
		span["parentSpanId"] = callerSpan
	}
	if e.Kind == channel.KindError {
		span["status"] = map[string]any{"code": 2, "message": in.ErrorMessage}
	}
	return span
}

// genAIAttrs maps a C5 attestation to GenAI semantic-convention attributes.
func genAIAttrs(attest json.RawMessage) []map[string]any {
	if len(attest) == 0 {
		return nil
	}
	var a struct {
		Engine string `json:"engine"`
		Model  string `json:"model"`
		Usage  *struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
			Cached *int64 `json:"cached_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(attest, &a) != nil || a.Model == "" {
		return nil
	}
	out := []map[string]any{
		StrAttr("gen_ai.system", a.Engine),
		StrAttr("gen_ai.request.model", a.Model),
	}
	if a.Usage != nil {
		if v := a.Usage.Input; v != nil && *v >= 0 {
			out = append(out, IntAttr("gen_ai.usage.input_tokens", *v))
		}
		if v := a.Usage.Output; v != nil && *v >= 0 {
			out = append(out, IntAttr("gen_ai.usage.output_tokens", *v))
		}
		if v := a.Usage.Cached; v != nil && *v >= 0 {
			out = append(out, IntAttr("gen_ai.usage.cached_input_tokens", *v))
		}
	}
	return out
}

// Document wraps spans in an OTLP/JSON export request.
func Document(resource []map[string]any, version string, spans []map[string]any) map[string]any {
	attrs := append([]map[string]any{StrAttr("service.name", "aura")}, resource...)
	return map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": map[string]any{"attributes": attrs},
			"scopeSpans": []map[string]any{{
				"scope": map[string]any{"name": "aura.causal-log", "version": version},
				"spans": spans,
			}},
		}},
	}
}

// StrAttr is an OTLP string attribute.
func StrAttr(key, value string) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"stringValue": value}}
}

// IntAttr is an OTLP integer attribute. OTLP/JSON carries int64 as a string.
func IntAttr(key string, value int64) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"intValue": fmt.Sprint(value)}}
}

// ChainTraces resolves the trace context of every envelope in a session by
// following cause links.
//
// Needed because the context is only guaranteed to be on the envelopes the
// kernel itself forwarded. What a skill emits carries it only if the skill's
// SDK echoes it, and every SDK released before v1.7 does not — so the answer
// for a skill's envelope is "whatever its cause had".
func ChainTraces(events []channel.Envelope) map[string]string {
	out := make(map[string]string, len(events))
	for _, e := range events {
		trace := ""
		if channel.ValidTraceparent(e.Trace) {
			trace = e.Trace
		} else if e.CauseID != "" {
			trace = out[e.CauseID]
		}
		if trace != "" {
			out[e.ID] = trace
		}
	}
	return out
}
