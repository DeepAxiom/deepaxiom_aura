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
)

// cmdDo — the full circle: natural-language goal → planner skill → IR graph
// → registered → executed, with human-approval gates answered in-terminal.
//
//	aura do "crea un usuario llamado Grace en el CRM"
//	aura do --yes "..."          auto-approve gates (scripting)
func cmdDo(args []string) {
	fs := flag.NewFlagSet("do", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	yes := fs.Bool("yes", false, "auto-approve human gates (careful)")
	planGraph := fs.String("plan-graph", "plan", "graph that reaches the planner")
	_ = fs.Parse(args)
	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		fatal(fmt.Errorf(`usage: aura do "natural-language goal"`))
	}

	// ── phase 1: request the plan ────────────────────────────────
	plan := requestPlan(*port, *planGraph, goal)
	if plan.Reasoning != "" {
		fmt.Printf("\n  plan › %s\n", plan.Reasoning)
	}
	var graphMeta struct {
		GraphID string `json:"graph_id"`
		Nodes   []struct {
			Ref     string `json:"ref"`
			Resolve string `json:"resolve"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(plan.Graph, &graphMeta)
	for _, n := range graphMeta.Nodes {
		fmt.Printf("  step › %s → %s\n", n.Ref, n.Resolve)
	}

	// ── phase 2: register the generated graph ───────────────────
	code, resp := postJSON(*port, "/v1/graphs", plan.Graph)
	if code != 201 {
		fatal(fmt.Errorf("the kernel rejected the plan graph (%d): %s", code, resp["error"]))
	}

	// ── phase 3: execute ─────────────────────────────────────────
	conn, err := newNodeClient(*port).dial("/v1/stream?graph=" + graphMeta.GraphID)
	if err != nil {
		fatal(err)
	}
	defer conn.Close()

	var hello channel.Envelope
	if err := conn.ReadJSON(&hello); err != nil {
		fatal(err)
	}
	if hello.Kind == channel.KindError {
		fatal(fmt.Errorf("session rejected: %s", string(hello.Payload)))
	}
	var ready struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(hello.Payload, &ready)
	fmt.Printf("  exec › session %s\n\n", ready.Session)

	for i, input := range plan.Inputs {
		env := channel.Envelope{
			V: channel.ProtocolMajor, ID: channel.NewID(),
			Node: "client", Port: input.Port, Seq: uint64(i + 1),
			Idem:   fmt.Sprintf("%s:client:%s:%d", ready.Session, input.Port, i+1),
			Schema: input.Schema, Kind: channel.KindData, Payload: input.Payload,
		}
		if err := conn.WriteJSON(env); err != nil {
			fatal(err)
		}
	}

	// receive: one data/done reply per input (or error), gates in between
	pending := len(plan.Inputs)
	stdin := bufio.NewScanner(os.Stdin)
	deadline := time.Now().Add(120 * time.Second)
	for pending > 0 && time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		var env channel.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			fatal(fmt.Errorf("connection lost while waiting for results: %w", err))
		}
		switch env.Kind {
		case channel.KindStatus:
			var body struct {
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(env.Payload, &body)
			if body.Detail != "" {
				fmt.Fprintf(os.Stderr, "  [·] %s\n", body.Detail)
			}
		case channel.KindConfirmRequest:
			var body struct {
				Question string `json:"question"`
			}
			_ = json.Unmarshal(env.Payload, &body)
			approve := *yes
			if *yes {
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
		case channel.KindData:
			pretty := prettyJSON(env.Payload)
			fmt.Printf("  result ‹ %s\n", pretty)
			pending--
		case channel.KindDone:
			pending--
		case channel.KindError:
			var body struct {
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(env.Payload, &body)
			fatal(fmt.Errorf("the plan failed: %s", body.Detail))
		}
	}
	if pending > 0 {
		fatal(fmt.Errorf("timeout waiting for %d result(s)", pending))
	}
	fmt.Println("\n  goal completed")
}

type planPayload struct {
	Reasoning string          `json:"reasoning"`
	Graph     json.RawMessage `json:"graph"`
	Inputs    []struct {
		Port    string          `json:"port"`
		Schema  string          `json:"schema"`
		Payload json.RawMessage `json:"payload"`
	} `json:"inputs"`
}

func requestPlan(port int, planGraph, goal string) *planPayload {
	conn, err := newNodeClient(port).dial("/v1/stream?graph=" + planGraph)
	if err != nil {
		fatal(err)
	}
	defer conn.Close()

	var hello channel.Envelope
	if err := conn.ReadJSON(&hello); err != nil {
		fatal(err)
	}
	if hello.Kind == channel.KindError {
		fatal(fmt.Errorf("is the planner skill running? — %s", string(hello.Payload)))
	}
	if err := conn.WriteJSON(map[string]string{"text": goal}); err != nil {
		fatal(err)
	}

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		var env channel.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			fatal(fmt.Errorf("connection lost while waiting for the plan: %w", err))
		}
		switch env.Kind {
		case channel.KindStatus:
			var body struct {
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(env.Payload, &body)
			if body.Detail != "" {
				fmt.Fprintf(os.Stderr, "  [·] %s\n", body.Detail)
			}
		case channel.KindData:
			if env.Schema == "std/plan@1" {
				var plan planPayload
				if err := json.Unmarshal(env.Payload, &plan); err != nil {
					fatal(fmt.Errorf("unreadable plan: %w", err))
				}
				return &plan
			}
		case channel.KindError:
			var body struct {
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(env.Payload, &body)
			fatal(fmt.Errorf("the planner failed: %s", body.Detail))
		}
	}
	fatal(fmt.Errorf("timeout waiting for the plan"))
	return nil
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "              ", "  ")
	return string(b)
}
