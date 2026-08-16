package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"aura/kernel/internal/channel"
	"aura/kernel/internal/ledger"
	"aura/kernel/internal/registry"
)

// cmdUndo — reverses one effect, or every reversible effect of a session, by
// re-delivering its original payload to the skill's declared compensation
// port (C1 `compensates`).
//
//	aura undo <session>          undo every undoable effect, reverse causal order
//	aura undo <receipt>          undo exactly one effect ("sha256:…")
//	aura undo <session> --yes    auto-approve human gates on the undo itself
//
// An undo is not a special code path: it is an ordinary graph edge —
// client.undo_out -> <skill>.<compensates.port> — driven over /v1/stream
// exactly like `aura do` drives a planner-generated graph, so it passes
// through the same policy and Effect Checkpoint every other delivery does
// (kernel/internal/executor.Session.forward). The kernel — not this command —
// is what refuses a second undo of the same receipt, an undo of an effect
// that was never delivered, or one with no declared compensation; see
// executor.validateUndo. This command's job is only to find the candidate
// entries, recover each one's original payload from the causal event log,
// and drive the confirm/result loop.
func cmdUndo(args []string) {
	fs := flag.NewFlagSet("undo", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	yes := fs.Bool("yes", false, "auto-approve human gates on the undo itself (careful)")
	// Flags may follow the target: `aura undo sess-x --yes` used to parse as
	// "no flags at all", which meant --yes was silently dropped on the one
	// command where dropping it changes whether a human is asked before an
	// effect is reversed. See parseWithOperands.
	rest := parseWithOperands(fs, args, 1)
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura undo <session|receipt> [--yes] [--port 9080]"))
	}
	target := strings.TrimPrefix(rest[0], "session:")

	var entries []ledger.Entry
	if strings.HasPrefix(target, "sha256:") {
		entries = []ledger.Entry{fetchLedgerEntry(*port, target)}
	} else {
		entries = fetchSessionLedger(*port, target)
		// Reverse causal order: undo the most recent effect first.
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}
	if len(entries) == 0 {
		fatal(fmt.Errorf("%s has no sealed effects", target))
	}

	fmt.Printf("\n  undo › %s (%d candidate effect(s))\n\n", target, len(entries))
	stdin := bufio.NewScanner(os.Stdin)
	undone, skipped := 0, 0
	for _, e := range entries {
		receipt := e.Hash()
		short := shortReceipt(receipt)
		switch {
		case e.Outcome != "delivered":
			fmt.Printf("  skip   %s  %-28s never delivered (outcome=%s)\n", short, e.Capability, e.Outcome)
			skipped++
			continue
		case e.Compensation == nil:
			fmt.Printf("  skip   %s  %-28s irreversible — no compensation declared\n", short, e.Capability)
			skipped++
			continue
		}
		if undoOne(*port, e, receipt, *yes, stdin) {
			fmt.Printf("  undone %s  %s\n", short, e.Capability)
			undone++
		} else {
			skipped++
		}
	}
	fmt.Printf("\n  undo: %d reversed, %d skipped\n", undone, skipped)
}

func shortReceipt(receipt string) string {
	h := strings.TrimPrefix(receipt, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return "sha256:" + h
}

func fetchLedgerEntry(port int, receipt string) ledger.Entry {
	code, body := getRaw(port, "/v1/ledger/entries/"+receipt)
	if code != 200 {
		fatal(fmt.Errorf("receipt %s not found", receipt))
	}
	var e ledger.Entry
	if err := json.Unmarshal(body, &e); err != nil {
		fatal(fmt.Errorf("unreadable ledger entry: %w", err))
	}
	return e
}

func fetchSessionLedger(port int, session string) []ledger.Entry {
	code, body := getRaw(port, "/v1/sessions/"+session+"/ledger")
	if code != 200 {
		fatal(fmt.Errorf("session %q not found, or this node has no effect ledger", session))
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

// undoOne drives one effect's reversal end to end: recover the original
// payload from the causal event log, resolve the compensating skill's live
// manifest for its ingress schema, register a one-edge ephemeral graph, and
// drive it over /v1/stream?undo=<receipt> exactly like `aura do` drives a
// planner-generated graph. Reports true only if the effect was actually
// delivered to the compensating port — a refusal from the kernel (already
// undone, policy deny, …) or a denied gate both count as "not undone".
func undoOne(port int, e ledger.Entry, receipt string, autoYes bool, stdin *bufio.Scanner) bool {
	original := findEnvelopePayload(port, e.Session, e.Envelope)
	if original == nil {
		fmt.Printf("  skip   %s  %-28s original payload not found in the event log\n",
			shortReceipt(receipt), e.Capability)
		return false
	}

	actorID := strings.SplitN(e.Actor, "@", 2)[0]
	m, ok := resolveLiveManifest(port, actorID)
	if !ok {
		fmt.Printf("  skip   %s  %-28s skill %q is not currently connected\n",
			shortReceipt(receipt), e.Capability, actorID)
		return false
	}
	schema, ok := m.IngressSchema(e.Compensation.Port)
	if !ok {
		fmt.Printf("  skip   %s  %-28s declared compensation port %q no longer exists on %q\n",
			shortReceipt(receipt), e.Capability, e.Compensation.Port, actorID)
		return false
	}

	graphID := "undo-" + strings.TrimPrefix(receipt, "sha256:")[:16]
	graph := map[string]any{
		"ir": "1", "graph_id": graphID,
		"origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{
			{"ref": "target", "use": actorID},
		},
		"edges": []map[string]any{
			{"from": "client.undo_out", "to": "target." + e.Compensation.Port},
		},
	}
	raw, _ := json.Marshal(graph)
	code, resp := postJSON(port, "/v1/graphs", raw)
	if code != 201 {
		fmt.Printf("  skip   %s  %-28s could not register undo graph: %v\n",
			shortReceipt(receipt), e.Capability, resp["error"])
		return false
	}

	conn, err := newNodeClient(port).dial("/v1/stream?graph=" + graphID + "&undo=" + receipt)
	if err != nil {
		fmt.Printf("  skip   %s  %-28s %v\n", shortReceipt(receipt), e.Capability, err)
		return false
	}
	defer conn.Close()

	var hello channel.Envelope
	if err := conn.ReadJSON(&hello); err != nil {
		fmt.Printf("  skip   %s  %-28s %v\n", shortReceipt(receipt), e.Capability, err)
		return false
	}
	if hello.Kind == channel.KindError {
		fmt.Printf("  skip   %s  %-28s refused: %s\n",
			shortReceipt(receipt), e.Capability, string(hello.Payload))
		return false
	}
	var ready struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(hello.Payload, &ready)

	out := channel.Envelope{
		V: channel.ProtocolMajor, ID: channel.NewID(), Session: ready.Session,
		Node: "client", Port: "undo_out", Seq: 1,
		Idem:   fmt.Sprintf("%s:client:undo_out:1", ready.Session),
		Schema: schema, Kind: channel.KindData, Payload: original,
	}
	if err := conn.WriteJSON(out); err != nil {
		fmt.Printf("  skip   %s  %-28s %v\n", shortReceipt(receipt), e.Capability, err)
		return false
	}

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		var env channel.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			fmt.Printf("  skip   %s  %-28s connection lost: %v\n", shortReceipt(receipt), e.Capability, err)
			return false
		}
		switch env.Kind {
		case channel.KindConfirmRequest:
			var body struct {
				Question string `json:"question"`
			}
			_ = json.Unmarshal(env.Payload, &body)
			approve := autoYes
			if autoYes {
				fmt.Printf("  gate › %s  → approved (--yes)\n", body.Question)
			} else {
				fmt.Printf("  gate › %s  [y/N]: ", body.Question)
				if stdin.Scan() {
					ans := strings.ToLower(strings.TrimSpace(stdin.Text()))
					approve = ans == "y" || ans == "yes" || ans == "s" || ans == "si"
				}
			}
			payload, _ := json.Marshal(map[string]bool{"approve": approve})
			_ = conn.WriteJSON(channel.Envelope{
				V: channel.ProtocolMajor, ID: channel.NewID(), CauseID: env.ID,
				Kind: channel.KindConfirmResponse, Payload: payload,
			})
		case channel.KindData, channel.KindDone:
			return true
		case channel.KindError:
			var body struct {
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(env.Payload, &body)
			fmt.Printf("  skip   %s  %-28s %s\n", shortReceipt(receipt), e.Capability, body.Detail)
			return false
		}
	}
	fmt.Printf("  skip   %s  %-28s timed out waiting for a result\n", shortReceipt(receipt), e.Capability)
	return false
}

// findEnvelopePayload recovers one envelope's payload from a session's causal
// event log by id — the source `aura undo` re-delivers to the compensating
// port, since C4 never stores the payload itself (only its sha256).
func findEnvelopePayload(port int, session, envelopeID string) json.RawMessage {
	for _, e := range fetchEvents(port, session) {
		if e.ID == envelopeID {
			return e.Payload
		}
	}
	return nil
}

// resolveLiveManifest finds a currently connected skill's manifest by exact
// package id — undo can only be delivered to a skill that is actually
// reachable right now.
func resolveLiveManifest(port int, id string) (registry.Manifest, bool) {
	code, body := getRaw(port, "/v1/skills")
	if code != 200 {
		return registry.Manifest{}, false
	}
	var skills []registry.Manifest
	if err := json.Unmarshal(body, &skills); err != nil {
		return registry.Manifest{}, false
	}
	for _, m := range skills {
		if m.ID == id {
			return m, true
		}
	}
	return registry.Manifest{}, false
}
