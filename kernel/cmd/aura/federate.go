package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"aura/kernel/internal/fed"
)

// cmdFederate — proxy a remote node's skills into the local node (H7).
//
//	aura federate <remote-url> [--capability <cap>] [--port 9080]
//
// The local node then resolves the remote capabilities as if they were local;
// each node keeps running standalone (leaf-node autonomy).
func cmdFederate(args []string) {
	fs := flag.NewFlagSet("federate", flag.ExitOnError)
	localPort := fs.Int("port", 9080, "local kernel port")
	capFilter := fs.String("capability", "", "only federate this capability (exact or prefix)")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) > 1 {
		_ = fs.Parse(rest[1:])
		rest = rest[:1]
	}
	if len(rest) != 1 {
		fatal(fmt.Errorf("usage: aura federate <remote-url> [--capability <cap>]"))
	}

	remoteHTTP := rest[0]
	if !strings.HasPrefix(remoteHTTP, "http") {
		remoteHTTP = "http://" + remoteHTTP
	}
	remoteWS := strings.Replace(remoteHTTP, "http", "ws", 1)

	bridge := &fed.Bridge{
		LocalWS:    fmt.Sprintf("ws://localhost:%d/ws/skill", *localPort),
		LocalHTTP:  fmt.Sprintf("http://localhost:%d", *localPort),
		RemoteHTTP: strings.TrimRight(remoteHTTP, "/"),
		RemoteWS:   strings.TrimRight(remoteWS, "/"),
		CapFilter:  *capFilter,
		// The local token is read from this machine's data dir like any other
		// CLI command. The remote one cannot be — it belongs to another node —
		// so it comes from the environment, which is also the only place it
		// could safely come from in a deployment.
		LocalToken:  nodeToken(""),
		RemoteToken: strings.TrimSpace(os.Getenv("AURA_REMOTE_TOKEN")),
		Log:         slog.New(slog.NewTextHandler(os.Stdout, nil)),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("federating %s into local node :%d — Ctrl+C to stop\n", remoteHTTP, *localPort)
	if err := bridge.Run(ctx); err != nil {
		fatal(err)
	}
}
