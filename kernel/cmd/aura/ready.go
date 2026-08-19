package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

// `aura ready` — the readiness probe, as a command.
//
// This exists for one caller that cannot use any of the others: a container
// healthcheck. The image is distroless, so there is no shell, no curl and no
// wget — the only executable in it is this binary, and a HEALTHCHECK therefore
// has to be an `aura` subcommand.
//
// `aura status` was standing in for it and was the wrong tool twice over. It
// asks for the skill catalogue, which needs a credential, and a container's
// probe should not have to hold one; and it exits 0 when that request is
// refused, because for a human at a terminal "the node is up but your token is
// wrong" is a useful thing to be told rather than a crash. A probe needs the
// opposite: one bit, and it has to mean "send this node traffic".
//
// So this reads `/readyz` — deliberately open for exactly this reason (see
// gateway.readiness) — and turns its status code into an exit code. Ready is 0.
// Anything else, including a node that is up but whose ledger did not open, is
// 1, and the failing checks are printed so `docker inspect` shows *why* rather
// than only that something was wrong.
func cmdReady(args []string) {
	fs := flag.NewFlagSet("ready", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	quiet := fs.Bool("quiet", false, "exit code only, print nothing")
	timeout := fs.Duration("timeout", 3*time.Second, "how long to wait for an answer")
	_ = fs.Parse(args)

	client := &http.Client{Timeout: *timeout}
	url := fmt.Sprintf("http://localhost:%d/readyz", *port)

	resp, err := client.Get(url)
	if err != nil {
		// Unreachable is not ready. Said plainly, because during a rolling
		// deploy this is the ordinary state for a few seconds and an operator
		// reading it should not think something broke.
		if !*quiet {
			fmt.Fprintf(os.Stderr, "not ready: %s is not answering (%v)\n", url, err)
		}
		os.Exit(1)
	}
	defer resp.Body.Close()

	var body struct {
		Ready  bool `json:"ready"`
		Checks []struct {
			Name   string `json:"name"`
			Ready  bool   `json:"ready"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	// A body we cannot parse is not a reason to call an otherwise-200 node
	// unready: the status code is the contract, the body is the explanation.
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if resp.StatusCode == http.StatusOK {
		if !*quiet {
			fmt.Println("ready")
			for _, c := range body.Checks {
				if !c.Ready || c.Detail != "" {
					fmt.Printf("  %-12s %s\n", c.Name, c.Detail)
				}
			}
		}
		return
	}

	if !*quiet {
		fmt.Fprintf(os.Stderr, "not ready (HTTP %d)\n", resp.StatusCode)
		for _, c := range body.Checks {
			if !c.Ready {
				fmt.Fprintf(os.Stderr, "  %-12s %s\n", c.Name, c.Detail)
			}
		}
	}
	os.Exit(1)
}
