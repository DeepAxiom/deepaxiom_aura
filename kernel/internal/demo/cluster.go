// Package demo runs a node under fire and shows what happens, measured.
//
// `aura demo` starts a real node and a real standby as separate processes — the
// same binary, the default security, a policy file — puts hundreds of live
// sessions through them, and then does six things to that system that a
// runtime has to survive: sessions that stop reading, a compromised Expertise
// reaching for a credential, orders that arrive too late, generations cut
// mid-sentence, the node killed outright, and one byte of the ledger edited on
// disk. Every number on the screen is measured in that run.
//
// # What is real and what is not
//
// The two nodes are real: `aura up` and `aura up --standby` on one data
// directory, spoken to over real sockets with the node's own token. Nothing in
// this package reaches into the kernel — it is a client of it, and the only
// thing it knows about the node that an outsider would not is where the data
// directory is, which it needs in order to attack the ledger file.
//
// The Expertise are real protocol clients, in this process: they register,
// receive deliveries and emit over a WebSocket exactly as an SDK does.
//
// The model is simulated unless --engine-url names a real one. The simulated
// engine speaks the OpenAI streaming API and produces tokens on a timer; it
// exists so the demo runs on a laptop with no GPU, and the screen says so. What
// is measured about inference here is the node's handling of it — placing,
// counting, cutting — never a model's speed.
//
// The service the effects act on is a small HTTP server in this process that
// accepts an order only with the right key. The key is stored in the node's
// credential broker and nowhere else.
package demo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// policyDoc is the policy both nodes run under.
//
// Two rules, and they are the whole security story the demo tells: dispatch
// orders are authorised by the operator, so they flow with no human in the
// path; payments are forbidden outright, whatever a graph says.
const policyDoc = `policy: 1
default_effect: gate
rules:
  - match: "motor.payments.*"
    decision: deny
    reason: "no automated payments on this node"
  - match: "motor.demo.*"
    decision: allow
    reason: "dispatch orders are authorised by the operator"
`

// secretName is the broker's name for the service key.
const secretName = "orders_api_key"

// cluster is the pair of node processes and the way to talk to them.
type cluster struct {
	binary    string
	dir       string // the data directory both nodes share
	logs      string
	port      int
	engineURL string
	engineKey string
	// enginePriority is the convention the node writes request priorities in,
	// or empty to write none. An engine not started to accept priorities
	// refuses a request that carries one.
	enginePriority string
	maxSessions    int

	token  string
	nodeID string
	http   *http.Client

	mu      sync.Mutex
	leader  *exec.Cmd
	standby *exec.Cmd
	all     []*exec.Cmd
}

func newCluster(binary string, port int, engineURL, engineKey, enginePriority string, maxSessions int) (*cluster, error) {
	root, err := os.MkdirTemp("", "aura-demo-")
	if err != nil {
		return nil, err
	}
	c := &cluster{
		binary: binary, port: port, engineURL: engineURL, engineKey: engineKey,
		enginePriority: enginePriority, maxSessions: maxSessions,
		dir: filepath.Join(root, "data"), logs: filepath.Join(root, "logs"),
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns: 64, MaxIdleConnsPerHost: 64, IdleConnTimeout: 30 * time.Second,
			},
		},
	}
	for _, d := range []string{c.dir, c.logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(c.policyPath(), []byte(policyDoc), 0o600); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *cluster) root() string       { return filepath.Dir(c.dir) }
func (c *cluster) policyPath() string { return filepath.Join(c.root(), "aura.policy.yaml") }
func (c *cluster) base() string       { return "http://127.0.0.1:" + strconv.Itoa(c.port) }
func (c *cluster) wsBase() string     { return "ws://127.0.0.1:" + strconv.Itoa(c.port) }

// spawn starts one node process. Default flags plus what the demo needs: the
// shared data directory, the policy, and where the engine is. Authentication is
// left on — a demo of what a node refuses, run on a node told to refuse
// nothing, would show nothing.
func (c *cluster) spawn(standby bool) (*exec.Cmd, error) {
	args := []string{"up",
		"--port", strconv.Itoa(c.port), "--data", c.dir,
		"--policy", c.policyPath(),
		"--max-sessions", strconv.Itoa(c.maxSessions),
		"--inference-url", c.engineURL,
	}
	if c.enginePriority != "" {
		args = append(args, "--inference-priority", c.enginePriority)
	}
	name := "leader"
	if standby {
		args = append(args, "--standby")
		name = "standby"
	}
	c.mu.Lock()
	n := len(c.all)
	c.mu.Unlock()
	logf, err := os.Create(filepath.Join(c.logs, fmt.Sprintf("%d-%s.log", n, name)))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(c.binary, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(os.Environ(), "AURA_ENGINE_API_KEY="+c.engineKey)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	go func() { _ = cmd.Wait(); logf.Close() }()
	c.mu.Lock()
	c.all = append(c.all, cmd)
	if standby {
		c.standby = cmd
	} else {
		c.leader = cmd
	}
	c.mu.Unlock()
	return cmd, nil
}

// start brings up the leader, learns its token and identity, stores the service
// key in its broker, and then starts the standby.
func (c *cluster) start(serviceKey string) error {
	if _, err := c.spawn(false); err != nil {
		return err
	}
	health, err := c.waitHealthy(30 * time.Second)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, c.tailLog(20))
	}
	c.nodeID, _ = health["node"].(string)

	tokenPath := filepath.Join(c.dir, "node.token")
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(tokenPath)
		if err == nil && len(bytes.TrimSpace(raw)) > 0 {
			c.token = string(bytes.TrimSpace(raw))
			break
		}
		if time.Now().After(deadline) {
			return errors.New("the node did not write its token")
		}
		time.Sleep(20 * time.Millisecond)
	}

	code, err := c.call(http.MethodPut, "/v1/secrets",
		map[string]string{"name": secretName, "value": serviceKey}, nil)
	if err != nil || code/100 != 2 {
		return fmt.Errorf("store the service key in the broker: status %d, %v", code, err)
	}
	if _, err := c.spawn(true); err != nil {
		return err
	}
	return nil
}

// waitHealthy polls /healthz, which needs no token, until the node answers.
func (c *cluster) waitHealthy(limit time.Duration) (map[string]any, error) {
	probe := &http.Client{Timeout: 400 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true}}
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		resp, err := probe.Get(c.base() + "/healthz")
		if err == nil {
			var out map[string]any
			ok := resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&out) == nil
			resp.Body.Close()
			if ok {
				return out, nil
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil, errors.New("the node never became healthy")
}

// call makes one authenticated request. body and out are JSON; either may be
// nil. The status is returned even when it is not a success, because several
// callers are checking that a request was refused.
func (c *cluster) call(method, path string, body, out any) (int, error) {
	return c.callAs(c.token, method, path, body, out)
}

func (c *cluster) callAs(token, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base()+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, nil
}

// registerGraph stores a graph, failing loudly: every beat depends on its
// graph existing, and a silent 500 here would look like a beat that failed for
// a reason it did not.
func (c *cluster) registerGraph(g map[string]any) error {
	var out map[string]any
	code, err := c.call(http.MethodPost, "/v1/graphs", g, &out)
	if err != nil {
		return err
	}
	if code != 200 && code != 201 {
		return fmt.Errorf("graph %v: status %d %v", g["graph_id"], code, out)
	}
	return nil
}

// killLeader ends the leading node the way a power cut or the OOM killer does:
// no signal it can handle, nothing flushed, no lease released. It returns the
// instant the kill was sent.
func (c *cluster) killLeader() (time.Time, error) {
	c.mu.Lock()
	leader := c.leader
	c.leader, c.standby = c.standby, nil
	c.mu.Unlock()
	if leader == nil || leader.Process == nil {
		return time.Time{}, errors.New("there is no leader to kill")
	}
	at := time.Now()
	if err := leader.Process.Kill(); err != nil {
		return at, err
	}
	return at, nil
}

// standbyWatching reports whether the standby has said who it stands by for.
// Until it has, killing the leader measures the standby's start-up, not its
// takeover.
func (c *cluster) standbyWatching() bool {
	return strings.Contains(c.readLog("standby"), "the writer is")
}

// tookOver returns the line in which the standby says why it took over.
func (c *cluster) tookOver() string {
	for _, line := range strings.Split(c.readLog("standby"), "\n") {
		if strings.Contains(line, "TOOK OVER") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func (c *cluster) readLog(kind string) string {
	matches, _ := filepath.Glob(filepath.Join(c.logs, "*-"+kind+".log"))
	var b strings.Builder
	for _, m := range matches {
		raw, _ := os.ReadFile(m)
		b.Write(raw)
	}
	return b.String()
}

func (c *cluster) tailLog(lines int) string {
	all := strings.Split(strings.TrimSpace(c.readLog("leader")+c.readLog("standby")), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// verifyOffline runs `aura verify` against the data directory: the check an
// auditor makes with no node running and no trust in one.
func (c *cluster) verifyOffline() (sound bool, output string, took time.Duration) {
	start := time.Now()
	out, err := exec.Command(c.binary, "verify", "--data", c.dir).CombinedOutput()
	return err == nil, string(out), time.Since(start)
}

// close stops every process and removes the working directory.
func (c *cluster) close(keep bool) {
	c.mu.Lock()
	procs := c.all
	c.mu.Unlock()
	for _, p := range procs {
		if p.Process != nil {
			_ = p.Process.Kill()
		}
	}
	// The nodes hold SQLite files open; give them a moment to be gone before
	// removing the tree, or the removal fails on platforms that lock open files.
	time.Sleep(300 * time.Millisecond)
	if !keep {
		_ = os.RemoveAll(c.root())
	}
}
