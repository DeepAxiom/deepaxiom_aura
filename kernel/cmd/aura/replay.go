package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
)

// cmdReplay — re-run a recorded session's real client inputs against the
// CURRENT version of its graph and diff the outputs (real traffic becomes
// an eval suite).
//
//	aura replay <session> [--port 9080] [--graph <override>] [--deny-gates]
//
// Gates encountered during replay are auto-approved (use --deny-gates to
// exercise the denial path). Model-backed graphs may legitimately differ;
// the diff is the point.
func cmdReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	graphOverride := fs.String("graph", "", "replay against a different graph (A/B)")
	denyGates := fs.Bool("deny-gates", false, "deny human-approval gates instead of approving")
	rest := parseWithOperands(fs, args, 1)
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura replay <session> [--graph <id>] [--deny-gates]"))
	}
	session := strings.TrimPrefix(rest[0], "session:")

	var meta struct {
		GraphID string `json:"graph_id"`
	}
	code, body := getRaw(*port, "/v1/sessions/"+session)
	if code != 200 {
		fatal(fmt.Errorf("session %q not found", session))
	}
	_ = json.Unmarshal(body, &meta)
	graph := meta.GraphID
	if *graphOverride != "" {
		graph = *graphOverride
	}

	// Split the recorded log: client roots (inputs) vs deliveries to client.
	events := fetchEvents(*port, session)
	var inputs, oldOutputs []channel.Envelope
	for _, e := range events {
		if e.Kind != channel.KindData || e.Node != "client" {
			continue
		}
		if e.CauseID == "" {
			inputs = append(inputs, e)
		} else {
			oldOutputs = append(oldOutputs, e)
		}
	}
	if len(inputs) == 0 {
		fatal(fmt.Errorf("session %s has no client inputs to replay", session))
	}
	fmt.Printf("\n  replaying %s → graph %s (%d input(s), expecting %d output(s))\n\n",
		session, graph, len(inputs), len(oldOutputs))

	// New session against the current graph.
	conn, err := newNodeClient(*port).dial("/v1/stream?graph=" + graph)
	if err != nil {
		fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(180 * time.Second)
	_ = conn.SetReadDeadline(deadline)

	var hello channel.Envelope
	if err := conn.ReadJSON(&hello); err != nil {
		fatal(err)
	}
	if hello.Kind == channel.KindError {
		fatal(fmt.Errorf("replay session rejected: %s", string(hello.Payload)))
	}
	var ready struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(hello.Payload, &ready)

	for i, input := range inputs {
		env := channel.Envelope{
			V: channel.ProtocolMajor, ID: channel.NewID(),
			Session: ready.Session, Node: "client", Port: input.Port,
			Seq:    uint64(i + 1),
			Idem:   fmt.Sprintf("replay:%s:%s:%d", session, input.Port, i+1),
			Schema: input.Schema, Kind: channel.KindData, Payload: input.Payload,
		}
		if err := conn.WriteJSON(env); err != nil {
			fatal(err)
		}
	}

	// Collect the same number of outputs the original produced.
	var newOutputs []channel.Envelope
	for len(newOutputs) < len(oldOutputs) && time.Now().Before(deadline) {
		var env channel.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			break
		}
		switch env.Kind {
		case channel.KindData:
			newOutputs = append(newOutputs, env)
		case channel.KindConfirmRequest:
			approve := !*denyGates
			verdict := "approved"
			if !approve {
				verdict = "denied"
			}
			fmt.Printf("  gate › auto-%s during replay\n", verdict)
			payload, _ := json.Marshal(map[string]bool{"approve": approve})
			_ = conn.WriteJSON(channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: env.ID,
				Kind: channel.KindConfirmResponse, Payload: payload,
			})
		case channel.KindError:
			newOutputs = append(newOutputs, env)
		}
	}

	// Diff old vs new, payload by payload.
	identical := 0
	max := len(oldOutputs)
	if len(newOutputs) > max {
		max = len(newOutputs)
	}
	for i := 0; i < max; i++ {
		oldP, newP := json.RawMessage("∅"), json.RawMessage("∅")
		if i < len(oldOutputs) {
			oldP = oldOutputs[i].Payload
		}
		if i < len(newOutputs) {
			newP = newOutputs[i].Payload
		}
		same := jsonEqual(oldP, newP)
		mark := "≠"
		if same {
			mark = "="
			identical++
		}
		fmt.Printf("  %s output %d\n", mark, i+1)
		if !same {
			fmt.Printf("      old ‹ %s\n", payloadPreview(oldP, 90))
			fmt.Printf("      new ‹ %s\n", payloadPreview(newP, 90))
		}
	}
	fmt.Printf("\n  replay session: %s\n", ready.Session)
	fmt.Printf("  verdict: %d/%d outputs identical", identical, len(oldOutputs))
	if identical == len(oldOutputs) && len(newOutputs) == len(oldOutputs) {
		fmt.Println("  ok")
	} else {
		fmt.Println("  (differences above — expected for model-backed graphs)")
	}

	printLedgerDiff(*port, session, ready.Session)
}

// fetchLedgerBySession returns one session's sealed ledger entries, in seq
// order — the same GET /v1/sessions/{id}/ledger endpoint `aura undo` reads.
// Empty rather than an error for a node with no ledger, or a session that
// sealed no effects at all (a graph with no motor skills, e.g. echo), so
// replaying either still completes cleanly.
func fetchLedgerBySession(port int, session string) []ledger.Entry {
	code, body := getRaw(port, "/v1/sessions/"+session+"/ledger")
	if code != 200 {
		return nil
	}
	var out struct {
		Entries []json.RawMessage `json:"entries"`
	}
	_ = json.Unmarshal(body, &out)
	entries := make([]ledger.Entry, 0, len(out.Entries))
	for _, raw := range out.Entries {
		var e ledger.Entry
		if json.Unmarshal(raw, &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries
}

// printLedgerDiff closes the fourth property of the project's thesis
// (ROADMAP.md): Reproducible. The payload diff above proves the conversation
// looked the same to a client; this proves the ledger agrees the
// *authorization* did — same capability, same policy decision, same outcome
// — using the ledger itself as the oracle, not the transcript.
func printLedgerDiff(port int, oldSession, newSession string) {
	oldEntries := fetchLedgerBySession(port, oldSession)
	newEntries := fetchLedgerBySession(port, newSession)
	if len(oldEntries) == 0 && len(newEntries) == 0 {
		return // nothing sealed either run — nothing to say
	}

	r := ledger.Diff(oldEntries, newEntries)
	fmt.Printf("\n  ledger: %d effect(s) sealed then, %d now", r.OldCount, r.NewCount)
	if r.Reproducible() {
		fmt.Println(" — reproducible: every capability, decision and outcome matched")
	} else {
		fmt.Println(" — NOT reproducible")
		for _, d := range r.Divergences {
			if d.Field == "count" {
				fmt.Printf("    ✗ effect count: %s then, %s now\n", d.Old, d.New)
				continue
			}
			fmt.Printf("    ✗ effect %d: %s changed %q -> %q\n", d.Index+1, d.Field, d.Old, d.New)
		}
	}
	for _, n := range r.Notes {
		fmt.Printf("    (effect %d: %s differs — %q -> %q; expected to vary run to run)\n",
			n.Index+1, n.Field, n.Old, n.New)
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return bytes.Equal(a, b)
	}
	ca, _ := json.Marshal(va)
	cb, _ := json.Marshal(vb)
	return bytes.Equal(ca, cb)
}
