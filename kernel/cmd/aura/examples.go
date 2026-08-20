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

	"gopkg.in/yaml.v3"

	"aura/kernel/internal/registry"
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
//
// # Orphans
//
// Skills dial *out* and reconnect forever by contract, so killing a node does
// not stop them: they keep retrying and attach to whatever node comes up next.
// Start a node with --with-examples on a machine that already has a set
// running and you get two of everything — a doubled palette, "2 candidates"
// on every node, and a round-robin quietly spreading sessions across two
// copies of the same skill.
//
// Running N replicas is legitimate and the registry rotates between them on
// purpose, so this is not something to refuse. It is something to *notice*.
// The launcher therefore asks the live catalogue what is already attached and
// leaves those alone, and after the probe window it says plainly if any skill
// ended up with more than one connection — which catches the orphan that
// reconnected while we were starting up, a race a pre-check cannot win.

// exampleProbe is how long a skill gets to fail before it is called started.
// Long enough for an ImportError, which happens at interpreter start; short
// enough that starting nine of them in parallel is not a pause anyone notices.
const exampleProbe = 4 * time.Second

type exampleResult struct {
	name    string
	started bool
	// attached marks a skill this launcher did not start because an instance
	// was already connected. Not a failure and not a start: the catalogue has
	// what it needs, and one fewer process is running than the flag implies.
	attached bool
	reason   string
}

// liveCatalog is the slice of the registry the launcher needs: what is
// attached right now. An interface so this file can be tested without a node.
type liveCatalog interface {
	Catalog() []registry.Manifest
}

// skillID reads a skill directory's declared id.
//
// Read rather than derived from the directory name: `skills/model-fit` is
// `example/sensorial/model-fit`, and any rule mapping one to the other is a
// rule that breaks the first time somebody renames a folder.
func skillID(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "skill.yaml"))
	if err != nil {
		return ""
	}
	var m struct {
		ID string `yaml:"id"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return m.ID
}

// attachedIDs counts live connections per skill id.
func attachedIDs(live liveCatalog) map[string]int {
	out := map[string]int{}
	if live == nil {
		return out
	}
	for _, m := range live.Catalog() {
		out[m.ID]++
	}
	return out
}

// replicas names every skill id holding more than one connection, sorted.
func replicas(live liveCatalog) []string {
	var out []string
	for id, n := range attachedIDs(live) {
		if n > 1 {
			out = append(out, fmt.Sprintf("%s (%d)", id, n))
		}
	}
	sort.Strings(out)
	return out
}

// startExamples launches every skill under dir, in parallel, and reports what
// happened. The returned func stops them; it is safe to call with no children.
func startExamples(dir, wsURL, token string, live liveCatalog) (func(), []exampleResult, []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return func() {}, []exampleResult{{
			name:   filepath.Base(dir),
			reason: fmt.Sprintf("no skills directory at %s — run from the repo root, or pass --examples-dir", dir),
		}}, nil
	}

	python := pythonBinary()
	if python == "" {
		return func() {}, []exampleResult{{
			name:   "python",
			reason: "no python3 on PATH; the example skills are Python programs",
		}}, nil
	}

	sdk, _ := filepath.Abs(filepath.Join(dir, "..", "sdk", "python", "src"))

	// Whatever is already attached, this launcher does not need to start.
	// Read once, before any child exists, so it describes the node as it was
	// found rather than as we are changing it.
	already := attachedIDs(live)

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

		if id := skillID(skillDir); id != "" && already[id] > 0 {
			// Someone else's process is serving this capability — most often
			// this machine's previous node, whose skills outlived it. Starting
			// a second one would work and would also be a lie about how many
			// processes the flag started.
			results = append(results, exampleResult{
				name:     e.Name(),
				attached: true,
				reason:   fmt.Sprintf("already connected as %s", id),
			})
			continue
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

	// Read the catalogue again now that the probe window has closed. An orphan
	// that was still backing off when we looked the first time has had its
	// chance to reconnect, and if it did, the pre-check could not have seen it.
	dupes := replicas(live)

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
	return stop, results, dupes
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

// reportExamples prints what started, what was already there, and what did
// not, in the banner's voice.
func reportExamples(results []exampleResult, dupes []string) {
	if len(results) == 0 {
		return
	}
	var started, attached, skipped []exampleResult
	for _, r := range results {
		switch {
		case r.started:
			started = append(started, r)
		case r.attached:
			attached = append(attached, r)
		default:
			skipped = append(skipped, r)
		}
	}

	fmt.Printf("\n  examples  %d started", len(started))
	if len(attached) > 0 {
		fmt.Printf(", %d already up", len(attached))
	}
	if len(skipped) > 0 {
		fmt.Printf(", %d skipped", len(skipped))
	}
	fmt.Println()
	for _, r := range started {
		fmt.Printf("            · %-18s started\n", r.name)
	}
	for _, r := range attached {
		fmt.Printf("            = %-18s %s\n", r.name, r.reason)
	}
	for _, r := range skipped {
		fmt.Printf("            × %-18s %s\n", r.name, r.reason)
	}
	if len(started) > 0 {
		fmt.Printf("            started means the process survived; `aura status` says which registered\n")
	}
	if len(attached) > 0 {
		fmt.Printf("            skills outlive the node that started them and reconnect to the next one;\n")
		fmt.Printf("            these were already here, so nothing new was launched for them\n")
	}
	if len(skipped) > 0 {
		fmt.Printf("            a skipped skill is a missing dependency, not a broken node\n")
	}
	reportReplicas(dupes)
}

// reportReplicas says when a skill ended up with more than one connection.
//
// Never an error: N replicas of a skill is a supported deployment and the
// registry round-robins between them deliberately. But when it happens by
// accident — an orphan from a previous node reconnecting mid-startup — the
// symptoms are a doubled palette and sessions landing on a process nobody
// remembers starting, and neither of those points at the cause.
func reportReplicas(dupes []string) {
	if len(dupes) == 0 {
		return
	}
	fmt.Printf("\n  replicas  %d skill(s) have more than one connection\n", len(dupes))
	for _, d := range dupes {
		fmt.Printf("            · %s\n", d)
	}
	fmt.Printf("            deliberate if you meant it — the registry rotates sessions between them.\n")
	fmt.Printf("            otherwise they outlived an earlier node; stop those processes and restart\n")
}
