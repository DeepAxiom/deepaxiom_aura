package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

// cmdWhy — explain a session by walking its causal event log backwards
// from the failure (or the last activity) to the root cause.
//
//	aura why [session] [--port 9080] [--no-explain]
//
// With no session argument it picks the most recent one. Narration uses
// the local LLM through the "chat" graph; if no LLM skill is connected it
// degrades to the deterministic chain (declared degradation, R15).
func cmdWhy(args []string) {
	fs := flag.NewFlagSet("why", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	noExplain := fs.Bool("no-explain", false, "skip the LLM narration")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) > 1 {
		_ = fs.Parse(rest[1:])
		rest = rest[:1]
	}

	session := ""
	if len(rest) == 1 {
		session = strings.TrimPrefix(rest[0], "session:")
	} else {
		session = latestSession(*port)
		fmt.Printf("  (no session given — using the most recent: %s)\n", session)
	}

	var meta struct {
		GraphID string `json:"graph_id"`
		Events  int    `json:"events"`
		Errors  int    `json:"errors"`
	}
	code, body := getRaw(*port, "/v1/sessions/"+session)
	if code != 200 {
		fatal(fmt.Errorf("session %q not found", session))
	}
	_ = json.Unmarshal(body, &meta)

	events := fetchEvents(*port, session)
	if len(events) == 0 {
		fatal(fmt.Errorf("session %s has no events", session))
	}
	byID := map[string]channel.Envelope{}
	for _, e := range events {
		byID[e.ID] = e
	}

	// Focus: the last error, or the last envelope if the session succeeded.
	var focus channel.Envelope
	failed := false
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == channel.KindError {
			focus, failed = events[i], true
			break
		}
	}
	if !failed {
		focus = events[len(events)-1]
	}

	// Walk the causal chain back to the root.
	chain := []channel.Envelope{focus}
	for cur := focus.CauseID; cur != ""; {
		parent, ok := byID[cur]
		if !ok {
			break
		}
		chain = append([]channel.Envelope{parent}, chain...)
		cur = parent.CauseID
	}

	fmt.Printf("\n  session %s · graph %s · %d envelopes · %d error(s)\n",
		session, meta.GraphID, meta.Events, meta.Errors)
	if failed {
		fmt.Printf("  FAILED — causal chain from root to failure:\n\n")
	} else {
		fmt.Printf("  no errors — causal chain of the last activity:\n\n")
	}
	for i, e := range chain {
		origin := e.Node
		if e.Port != "" {
			origin += "." + e.Port
		}
		marker := "│"
		if i == len(chain)-1 && failed {
			marker = "x"
		}
		fmt.Printf("  %s %2d. [%-15s] %-22s %s\n",
			marker, i+1, e.Kind, origin, payloadPreview(e.Payload, 60))
	}

	if *noExplain {
		return
	}
	explanation := narrate(*port, session, meta.GraphID, chain, failed)
	if explanation != "" {
		fmt.Printf("\n  why › %s\n", explanation)
	} else {
		fmt.Printf("\n  (no LLM skill connected — start skills/llm-chat for a narrated explanation)\n")
	}
}

// narrate asks the local LLM (through the "chat" graph) for a root-cause
// explanation. Returns "" when no LLM is reachable — the caller degrades.
func narrate(port int, session, graph string, chain []channel.Envelope, failed bool) string {
	var sb strings.Builder
	state := "succeeded"
	if failed {
		state = "FAILED"
	}
	fmt.Fprintf(&sb, "You are `aura why`, a runtime debugger. Session %s on graph %q %s. "+
		"This is the causal chain of message envelopes (root first). "+
		"Explain the root cause in plain language, in 2-3 sentences, in the language "+
		"of the user-visible payloads. Do not repeat ids.\n\n", session, graph, state)
	for i, e := range chain {
		fmt.Fprintf(&sb, "%d. kind=%s origin=%s.%s payload=%s\n",
			i+1, e.Kind, e.Node, e.Port, payloadPreview(e.Payload, 200))
	}

	url := fmt.Sprintf("ws://localhost:%d/v1/stream?graph=chat", port)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return ""
	}
	defer conn.Close()
	deadline := time.Now().Add(120 * time.Second)
	_ = conn.SetReadDeadline(deadline)

	var hello channel.Envelope
	if conn.ReadJSON(&hello) != nil || hello.Kind == channel.KindError {
		return ""
	}
	if conn.WriteJSON(map[string]string{"text": sb.String()}) != nil {
		return ""
	}
	var out strings.Builder
	for time.Now().Before(deadline) {
		var env channel.Envelope
		if conn.ReadJSON(&env) != nil {
			break
		}
		switch env.Kind {
		case channel.KindData:
			var body struct {
				Text  string `json:"text"`
				Final bool   `json:"final"`
			}
			_ = json.Unmarshal(env.Payload, &body)
			out.WriteString(body.Text)
			if body.Final {
				return strings.TrimSpace(out.String())
			}
		case channel.KindDone:
			return strings.TrimSpace(out.String())
		case channel.KindError:
			return ""
		}
	}
	return strings.TrimSpace(out.String())
}

// ── shared helpers for why/replay ────────────────────────────────

func latestSession(port int) string {
	var out struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	code, body := getRaw(port, "/v1/sessions")
	if code != 200 {
		fatal(fmt.Errorf("kernel not reachable on :%d — run `aura up` first", port))
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Sessions) == 0 {
		fatal(fmt.Errorf("no sessions recorded yet"))
	}
	return out.Sessions[0].SessionID
}

func fetchEvents(port int, session string) []channel.Envelope {
	code, body := getRaw(port, "/v1/sessions/"+session+"/events")
	if code != 200 {
		fatal(fmt.Errorf("could not fetch events for %s", session))
	}
	var out struct {
		Events []json.RawMessage `json:"events"`
	}
	_ = json.Unmarshal(body, &out)
	events := make([]channel.Envelope, 0, len(out.Events))
	for _, raw := range out.Events {
		var e channel.Envelope
		if json.Unmarshal(raw, &e) == nil {
			events = append(events, e)
		}
	}
	return events
}

func payloadPreview(raw json.RawMessage, max int) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func getRaw(port int, path string) (int, []byte) {
	return newNodeClient(port).raw(http.MethodGet, path, nil)
}
