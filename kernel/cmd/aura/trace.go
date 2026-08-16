package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"aura/kernel/internal/channel"
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
func spanID(envelopeID string) string {
	sum := sha256.Sum256([]byte("span:" + envelopeID))
	return hex.EncodeToString(sum[:8])
}

func traceID(session string) string {
	sum := sha256.Sum256([]byte("trace:" + session))
	return hex.EncodeToString(sum[:16])
}

func buildOTLP(session string, events []json.RawMessage, times []int64) map[string]any {
	tid := traceID(session)
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
	for i, e := range parsed {
		ts := time.Now().UnixMilli()
		if i < len(times) && times[i] > 0 {
			ts = times[i]
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
		span := map[string]any{
			"traceId":           tid,
			"spanId":            spanID(e.ID),
			"name":              name,
			"kind":              1, // SPAN_KIND_INTERNAL
			"startTimeUnixNano": fmt.Sprint(startNano),
			"endTimeUnixNano":   fmt.Sprint(startNano + int64(time.Millisecond)),
			"attributes": []map[string]any{
				strAttr("aura.kind", e.Kind),
				strAttr("aura.node", e.Node),
				strAttr("aura.port", e.Port),
				strAttr("aura.schema", e.Schema),
				strAttr("aura.envelope_id", e.ID),
				strAttr("aura.payload", payloadPreview(e.Payload, 200)),
			},
		}
		// Parent span = causal parent, when it exists in this session.
		if e.CauseID != "" && known[e.CauseID] {
			span["parentSpanId"] = spanID(e.CauseID)
		}
		if e.Kind == channel.KindError {
			span["status"] = map[string]any{"code": 2, "message": payloadPreview(e.Payload, 120)}
		}
		spans = append(spans, span)
	}
	return map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": map[string]any{"attributes": []map[string]any{
				strAttr("service.name", "aura"),
				strAttr("aura.session", session),
			}},
			"scopeSpans": []map[string]any{{
				"scope": map[string]any{"name": "aura.causal-log", "version": version},
				"spans": spans,
			}},
		}},
	}
}

func strAttr(key, value string) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"stringValue": value}}
}
