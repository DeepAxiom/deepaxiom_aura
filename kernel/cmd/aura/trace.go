package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/otel"
)

// cmdTrace — export a session's causal event log as OpenTelemetry traces
// (the borders are standards). One span per envelope, with
// parent = cause_id, so any OTel backend renders the causal tree.
//
//	aura trace <session> [--otlp http://collector:4318] [--out file.json] [--port 9080]
//
// With --otlp it POSTs OTLP/JSON to <url>/v1/traces; otherwise it writes
// the OTLP document to --out (default aura-trace-<session>.json).
func cmdTrace(args []string) {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	otlp := fs.String("otlp", "", "OTLP/HTTP collector base URL (e.g. http://localhost:4318)")
	out := fs.String("out", "", "write the OTLP JSON document to this file")
	rest := parseWithOperands(fs, args, 1)
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura trace <session> [--otlp <url>] [--out <file>]"))
	}
	session := strings.TrimPrefix(rest[0], "session:")

	code, body := getRaw(*port, "/v1/sessions/"+session+"/events")
	if code != 200 {
		fatal(fmt.Errorf("session %q not found", session))
	}
	var log struct {
		Events     []json.RawMessage `json:"events"`
		Timestamps []int64           `json:"timestamps"`
	}
	_ = json.Unmarshal(body, &log)
	if len(log.Events) == 0 {
		fatal(fmt.Errorf("session %s has no events", session))
	}

	doc := buildOTLP(session, log.Events, log.Timestamps)
	payload, _ := json.MarshalIndent(doc, "", "  ")

	if *otlp != "" {
		resp, err := http.Post(strings.TrimRight(*otlp, "/")+"/v1/traces",
			"application/json", bytes.NewReader(payload))
		if err != nil {
			fatal(fmt.Errorf("collector not reachable: %w", err))
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			fatal(fmt.Errorf("collector rejected the export: HTTP %d", resp.StatusCode))
		}
		fmt.Printf("exported %d span(s) of session %s to %s/v1/traces (HTTP %d)\n",
			len(log.Events), session, *otlp, resp.StatusCode)
		return
	}

	file := *out
	if file == "" {
		file = "aura-trace-" + session + ".json"
	}
	if err := os.WriteFile(file, payload, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %d span(s) of session %s to %s (OTLP/JSON)\n",
		len(log.Events), session, file)
}

// spanID derives a stable OTel id from an envelope id (16 hex chars);
// traceID from the session (32 hex chars). OTLP/JSON encodes ids as hex.
func spanID(envelopeID string) string { return otel.SpanID(envelopeID) }

func traceID(session string) string { return otel.TraceID(session) }

func buildOTLP(session string, events []json.RawMessage, times []int64) map[string]any {
	spans := make([]map[string]any, 0, len(events))
	known := map[string]bool{}
	var parsed []channel.Envelope
	for _, raw := range events {
		var e channel.Envelope
		if json.Unmarshal(raw, &e) == nil {
			parsed = append(parsed, e)
			known[e.ID] = true
		}
	}
	// C3 v1.7: a chain whose client supplied a trace context is exported
	// under that trace, so it joins the caller's instead of standing alone.
	traces := otel.ChainTraces(parsed)
	for i, e := range parsed {
		var ts int64
		if i < len(times) && times[i] > 0 {
			ts = times[i]
		}
		in := otel.SpanInput{
			Session: session, Envelope: e, TSMillis: ts,
			// Parent span = causal parent, when it exists in this session.
			ParentKnown: e.CauseID != "" && known[e.CauseID],
			Trace:       traces[e.ID],
			Payload:     payloadPreview(e.Payload, 200),
		}
		if e.Kind == channel.KindError {
			in.ErrorMessage = payloadPreview(e.Payload, 120)
		}
		spans = append(spans, otel.Span(in))
	}
	return otel.Document(
		[]map[string]any{strAttr("aura.session", session)}, version, spans)
}

func strAttr(key, value string) map[string]any { return otel.StrAttr(key, value) }

// firstNonEmpty returns the first argument that is not blank.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
