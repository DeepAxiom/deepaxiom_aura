package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"aura/kernel/internal/channel"
)

// cmdChat — talk to a running graph from the terminal.
//
//	aura chat                  interactive REPL against graph "chat"
//	aura chat "hola"           one-shot message, prints reply, exits
//	aura chat --graph echo     use another graph
func cmdChat(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	graph := fs.String("graph", "chat", "graph to talk to")
	_ = fs.Parse(args)
	oneShot := strings.TrimSpace(strings.Join(fs.Args(), " "))

	conn, err := newNodeClient(*port).dial("/v1/stream?graph=" + *graph)
	if err != nil {
		fatal(err)
	}
	defer conn.Close()

	// Wait for the session-ready status frame.
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

	send := func(text string) {
		if err := conn.WriteJSON(map[string]string{"text": text}); err != nil {
			fatal(err)
		}
	}

	// receive prints streamed envelopes until a final chunk / done arrives.
	receive := func() {
		printed := false
		for {
			var env channel.Envelope
			if err := conn.ReadJSON(&env); err != nil {
				fmt.Println()
				fatal(fmt.Errorf("connection lost: %w", err))
			}
			switch env.Kind {
			case channel.KindData:
				var body struct {
					Text  string `json:"text"`
					Final bool   `json:"final"`
				}
				_ = json.Unmarshal(env.Payload, &body)
				fmt.Print(body.Text)
				printed = true
				if body.Final {
					fmt.Println()
					return
				}
			case channel.KindDone:
				if printed {
					fmt.Println()
				}
				return
			case channel.KindStatus:
				var body struct {
					State  string `json:"state"`
					Detail string `json:"detail"`
				}
				_ = json.Unmarshal(env.Payload, &body)
				if body.Detail != "" {
					fmt.Fprintf(os.Stderr, "  [%s] %s\n", body.State, body.Detail)
				}
			case channel.KindError:
				var body struct {
					Detail string `json:"detail"`
				}
				_ = json.Unmarshal(env.Payload, &body)
				fmt.Fprintf(os.Stderr, "\n  error: %s\n", body.Detail)
				return
			}
		}
	}

	if oneShot != "" {
		send(oneShot)
		receive()
		return
	}

	fmt.Printf("aura chat — graph %q, session %s (Ctrl+C to exit)\n\n", *graph, ready.Session)
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("you › ")
		if !scanner.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		send(line)
		fmt.Print("aura › ")
		receive()
		fmt.Println()
	}
}
