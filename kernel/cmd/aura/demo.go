package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aura/kernel/internal/demo"
)

// cmdDemo runs a node under fire and shows, or prints, what happened.
//
// It starts its own nodes from this same binary, on its own ports and in its
// own temporary data directory, and removes all of it when it ends. It never
// touches a node already running on this machine.
func cmdDemo(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7700", "address the screen is served on")
	nodePort := fs.Int("node-port", 7711, "port the demo's own node listens on")
	sessions := fs.Int("sessions", 300, "relay sessions: each sends at --rate and times the answer")
	ops := fs.Int("ops", 24, "operation sessions: model call, order, sealed effect, call to the service")
	rate := fs.Float64("rate", 10, "messages per second per relay session")
	pace := fs.Duration("pace", 6*time.Second, "quiet time before each beat")
	warmup := fs.Duration("warmup", 5*time.Second, "time at steady load before the first beat")
	headless := fs.Bool("headless", false,
		"no screen: run at once, print each result, exit non-zero if anything failed")
	now := fs.Bool("now", false, "with a screen, start at once instead of when someone opens it")
	asJSON := fs.Bool("json", false, "print the final state as JSON")
	engineURL := fs.String("engine-url", "",
		"a real OpenAI-compatible engine to use instead of the simulated one (its key: $AURA_ENGINE_API_KEY)")
	engineModel := fs.String("engine-model", "", "model name to ask a real engine for")
	enginePriority := fs.String("engine-priority", "",
		"how a real engine wants priorities written: vllm or sglang (default: none are sent)")
	keep := fs.Bool("keep", false, "leave the working directory behind: logs, policy and the ledger")
	lang := fs.String("lang", "", "language of the screen: en or es (default: the browser's)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `aura demo — a node under fire, measured

Starts a real node and its standby, opens hundreds of live sessions through
them, and then does six things to that system:

  1  sessions stop reading while an Expertise keeps sending to them
  2  a compromised Expertise tries every way it has to reach a service
  3  orders arrive after their deadline
  4  long generations are cancelled mid-sentence
  5  the node is killed with SIGKILL
  6  one byte of the ledger is edited on disk

Every number shown is measured in that run. The model is simulated unless
--engine-url names a real one, and the screen says which.

    aura demo                      open http://127.0.0.1:7700 and watch
    aura demo --headless           no screen; exit code says whether it held

`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	opt := demo.Options{
		Binary: demo.Executable(), Version: version,
		NodePort: *nodePort, Sessions: *sessions, Ops: *ops, Rate: *rate,
		Pace: *pace, Warmup: *warmup,
		EngineURL: *engineURL, EngineKey: os.Getenv("AURA_ENGINE_API_KEY"), EngineModel: *engineModel,
		EnginePriority: *enginePriority,
		Hold:           !*headless && !*now, Keep: *keep,
		Out: os.Stderr,
	}
	if *ops == 0 {
		opt.Ops = -1 // asked for none, as opposed to not asked
	}
	run := demo.New(opt)
	defer run.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var screen *demo.Screen
	if !*headless {
		var err error
		screen, err = demo.Serve(run, *listen, *lang)
		if err != nil {
			fmt.Fprintln(os.Stderr, "aura demo:", err)
			os.Exit(1)
		}
		defer screen.Close()
		fmt.Fprintf(os.Stderr, "\n  the screen is at  http://%s\n", screen.Addr())
		if opt.Hold {
			fmt.Fprintf(os.Stderr, "  the run starts when it is opened (or pass --now)\n")
		}
		fmt.Fprintln(os.Stderr)
	}

	err := run.Start(ctx)
	snap := run.Snapshot()
	if *asJSON {
		out, _ := json.MarshalIndent(snap, "", "  ")
		fmt.Println(string(out))
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintln(os.Stderr, "\naura demo:", err)
		run.Close()
		os.Exit(1)
	}

	survived := 0
	for _, b := range snap.Beats {
		if b.Status == "survived" {
			survived++
		}
	}
	fmt.Fprintf(os.Stderr, "\n  %d of %d survived\n", survived, len(snap.Beats))
	if *headless {
		if !snap.Survived() {
			run.Close()
			os.Exit(1)
		}
		return
	}
	// With a screen, the result stays up until the viewer is done with it. The
	// nodes and the load do not: they are stopped now, and what is shown is the
	// run as it ended.
	run.Close()
	fmt.Fprintln(os.Stderr, "  the result stays on the screen; Ctrl-C to stop")
	<-ctx.Done()
}
