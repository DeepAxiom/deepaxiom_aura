package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// `aura up --with-examples` — start the example skills alongside the node.
//
// A fresh node is a working kernel with an empty catalogue, and that is the
// right default: skills are separate processes that connect *to* the kernel,
// the kernel is a Go binary that should not depend on a Python toolchain, and
// a node in production runs skills an operator chose. This flag exists because
// that default is also a bad first five minutes — you start the runtime, open
// the UI, and there is nothing to do until you have read enough documentation
// to know what to launch by hand.
//
// So: opt-in, never the default, and honest about what it could not start.
// Half the examples need packages that compile native code (llama-cpp-python,
// faster-whisper) or a database that is not there (postgres-cdc). Starting
// those on a machine without them means a crash loop in the log; the interesting
// question is not whether they run but *why not*, and the answer is a pip line.
//
// Rather than map package names to import names — a table that is wrong the day
// a dependency is renamed — each skill is launched and watched. One that is
// still alive after a moment is working; one that died hands back its own last
// line of stderr, which is a better diagnosis than anything guessed in advance.

// exampleProbe is how long a skill gets to fail before it is called started.
// Long enough for an ImportError, which happens at interpreter start; short
// enough that starting nine of them in parallel is not a pause anyone notices.
const exampleProbe = 4 * time.Second

type exampleResult struct {
	name    string
	started bool
	reason  string
}

// startExamples launches every skill under dir, in parallel, and reports what
// happened. The returned func stops them; it is safe to call with no children.
func startExamples(dir, wsURL, token string) (func(), []exampleResult) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return func() {}, []exampleResult{{
			name:   filepath.Base(dir),
			reason: fmt.Sprintf("no skills directory at %s — run from the repo root, or pass --examples-dir", dir),
		}}
	}

	python := pythonBinary()
	if python == "" {
		return func() {}, []exampleResult{{
			name:   "python",
			reason: "no python3 on PATH; the example skills are Python programs",
		}}
	}

	sdk, _ := filepath.Abs(filepath.Join(dir, "..", "sdk", "python", "src"))

	var (
		mu      sync.Mutex
		results []exampleResult
		procs   []*exec.Cmd
		wg      sync.WaitGroup
	)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(skillDir, "main.py")); err != nil {
			continue // not a runnable skill: skills/README.md, LICENSE, …
		}

		wg.Add(1)
		go func(name, path string) {
			defer wg.Done()
			cmd, res := launchExample(python, path, name, sdk, wsURL, token)
			mu.Lock()
			results = append(results, res)
			if cmd != nil {
				procs = append(procs, cmd)
			}
			mu.Unlock()
		}(e.Name(), skillDir)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		if results[i].started != results[j].started {
			return results[i].started // working ones first
		}
		return results[i].name < results[j].name
	})

	stop := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range procs {
			if c.Process != nil {
				// The skills reconnect forever by contract, so they will not
				// stop on their own when the node does.
				_ = c.Process.Kill()
			}
		}
	}
	return stop, results
}

// launchExample starts one skill and waits to see whether it survives.
func launchExample(python, dir, name, sdk, wsURL, token string) (*exec.Cmd, exampleResult) {
	cmd := exec.Command(python, "main.py")
	// The SDK loads skill.yaml from the working directory, so this has to be
	// the skill's own directory — starting from the repo root cannot find it.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+sdk,
		"AURA_WS_URL="+wsURL,
		// Unbuffered, or a skill that dies immediately produces no stderr to
		// report because Python never flushed it.
		"PYTHONUNBUFFERED=1",
	)
	if token != "" {
		cmd.Env = append(cmd.Env, "AURA_TOKEN="+token)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, exampleResult{name: name, reason: err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return nil, exampleResult{name: name, reason: err.Error()}
	}

	// Keep the tail of stderr: a dead skill's last line is the diagnosis, and
	// a live one's output would otherwise fill a pipe and block it.
	var last string
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			if text == "" {
				continue
			}
			select {
			case <-lines: // drop the previous one, keep only the newest
			default:
			}
			select {
			case lines <- text:
			default:
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-time.After(exampleProbe):
		// Alive, which is not the same as registered: a skill can survive its
		// imports and still be retrying against something that is not there —
		// postgres-cdc without a database is the obvious one. The banner says
		// "started" rather than "connected" for that reason, and points at the
		// catalogue, which is the only place registration is a fact.
		return cmd, exampleResult{name: name, started: true}
	case <-done:
		select {
		case last = <-lines:
		default:
		}
		return nil, exampleResult{name: name, reason: diagnose(last)}
	}
}

// diagnose turns a Python traceback's last line into something actionable.
func diagnose(stderr string) string {
	if stderr == "" {
		return "exited immediately with no output"
	}
	// ModuleNotFoundError: No module named 'llama_cpp'
	if i := strings.Index(stderr, "No module named "); i >= 0 {
		module := strings.Trim(stderr[i+len("No module named "):], "'\" ")
		return fmt.Sprintf("needs %s — install its requirements.txt", module)
	}
	if len(stderr) > 160 {
		stderr = stderr[:160] + "…"
	}
	return stderr
}

func pythonBinary() string {
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// reportExamples prints what started and what did not, in the banner's voice.
func reportExamples(results []exampleResult) {
	if len(results) == 0 {
		return
	}
	var started, skipped []exampleResult
	for _, r := range results {
		if r.started {
			started = append(started, r)
		} else {
			skipped = append(skipped, r)
		}
	}

	fmt.Printf("\n  examples  %d started", len(started))
	if len(skipped) > 0 {
		fmt.Printf(", %d skipped", len(skipped))
	}
	fmt.Println()
	for _, r := range started {
		fmt.Printf("            · %-18s started\n", r.name)
	}
	for _, r := range skipped {
		fmt.Printf("            × %-18s %s\n", r.name, r.reason)
	}
	if len(started) > 0 {
		fmt.Printf("            started means the process survived; `aura status` says which registered\n")
	}
	if len(skipped) > 0 {
		fmt.Printf("            a skipped skill is a missing dependency, not a broken node\n")
	}
}
