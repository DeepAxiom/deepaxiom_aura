package demo

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The ordinary agent: what `aura demo --versus` compares a node against.
//
// It is one process that does the whole job the way most agents are built. It
// takes requests from clients over a WebSocket, asks the model, decides, calls
// the service with a key from its environment, writes what it did to a log
// file, and answers. It is not a bad agent, and it is not written to lose:
//
//   - it has its own rule about what it will not do, and enforces it;
//   - it remembers which requests it has already carried out, so a client
//     that asks twice is not served twice;
//   - it keeps an audit log, and can check that log for damage.
//
// What it does not have is anything outside itself. Its rule, its memory and
// its key are in the one process that an attacker takes over and that a crash
// takes down, and its log is a file that says whatever was last written to it.
// Those four facts are the whole comparison; nothing else about it is rigged.
//
// It runs as its own process (`aura demo-plain`, started by the demo) so that
// killing it is killing a process, exactly as it is for the node.

// PlainConfig is the agent's configuration, which it reads from its
// environment like any service does.
type PlainConfig struct {
	Listen     string
	EngineURL  string
	EngineKey  string
	ServiceURL string
	ServiceKey string
	LogPath    string
}

// PlainConfigFromEnv reads the configuration the demo started the agent with.
func PlainConfigFromEnv() PlainConfig {
	return PlainConfig{
		Listen:     os.Getenv("PLAIN_LISTEN"),
		EngineURL:  os.Getenv("PLAIN_ENGINE_URL"),
		EngineKey:  os.Getenv("PLAIN_ENGINE_KEY"),
		ServiceURL: os.Getenv("PLAIN_SERVICE_URL"),
		ServiceKey: os.Getenv("PLAIN_SERVICE_KEY"),
		LogPath:    os.Getenv("PLAIN_LOG"),
	}
}

func (c PlainConfig) env() []string {
	return []string{
		"PLAIN_LISTEN=" + c.Listen, "PLAIN_ENGINE_URL=" + c.EngineURL, "PLAIN_ENGINE_KEY=" + c.EngineKey,
		"PLAIN_SERVICE_URL=" + c.ServiceURL, "PLAIN_SERVICE_KEY=" + c.ServiceKey, "PLAIN_LOG=" + c.LogPath,
	}
}

// plainMsg is a request from a client; plainReply is the answer.
type plainMsg struct {
	ID      string `json:"id"`
	Op      string `json:"op"` // work | late
	Order   string `json:"order"`
	DelayMS int    `json:"delay_ms,omitempty"`
}

type plainReply struct {
	Re    string `json:"re"`
	Done  string `json:"done,omitempty"`
	Turn  int    `json:"turn,omitempty"`
	Error string `json:"error,omitempty"`
}

// forbiddenPrefix marks the orders neither system is allowed to carry out.
const forbiddenPrefix = "pay-"

type plainAgent struct {
	cfg  PlainConfig
	http *http.Client

	mu    sync.Mutex
	turns map[string]int    // session -> how many requests it has been answered
	seen  map[string]string // request id -> the order carried out for it
	log   *os.File
	seq   int
}

// RunPlainAgent serves until the process is killed.
func RunPlainAgent(cfg PlainConfig) error {
	if cfg.Listen == "" {
		return errors.New("the plain agent is started by `aura demo --versus`, with its configuration in PLAIN_* variables")
	}
	_, handler, err := newPlainAgent(cfg)
	if err != nil {
		return err
	}
	return http.ListenAndServe(cfg.Listen, handler)
}

// newPlainAgent builds the agent and the handler that serves it.
func newPlainAgent(cfg PlainConfig) (*plainAgent, http.Handler, error) {
	if cfg.ServiceURL == "" || cfg.LogPath == "" {
		return nil, nil, errors.New("the plain agent is started by `aura demo --versus`, with its configuration in PLAIN_* variables")
	}
	a := &plainAgent{cfg: cfg, turns: map[string]int{}, seen: map[string]string{},
		http: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}}
	// The log outlives the process; the count of what is in it is how the
	// next line gets its number.
	if raw, err := os.ReadFile(cfg.LogPath); err == nil {
		a.seq = bytes.Count(raw, []byte("\n"))
	}
	f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	a.log = f

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("GET /session", a.session)
	mux.HandleFunc("GET /check", a.check)
	mux.HandleFunc("POST /taken-over", a.takenOver)
	return a, mux, nil
}

var plainUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (a *plainAgent) session(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("id")
	conn, err := plainUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var wmu sync.Mutex
	for {
		var m plainMsg
		if err := conn.ReadJSON(&m); err != nil {
			return
		}
		go func() {
			reply := a.handle(session, m)
			wmu.Lock()
			_ = conn.WriteJSON(reply)
			wmu.Unlock()
		}()
	}
}

func (a *plainAgent) handle(session string, m plainMsg) plainReply {
	reply := plainReply{Re: m.ID}
	// A request it has already carried out is answered, not carried out again.
	a.mu.Lock()
	order, done := a.seen[m.ID]
	a.mu.Unlock()
	if done {
		reply.Done, reply.Turn = order, a.turn(session, false)
		return reply
	}

	switch m.Op {
	case "work":
		a.model(12) // decide
	case "late":
		// The planning that came before the act took this long. Nothing
		// between here and the service knows how old the decision is by now.
		time.Sleep(time.Duration(m.DelayMS) * time.Millisecond)
	default:
		reply.Error = "unknown request"
		return reply
	}
	if err := a.act(session, m.Order); err != nil {
		reply.Error = err.Error()
		return reply
	}
	a.mu.Lock()
	a.seen[m.ID] = m.Order
	a.mu.Unlock()
	if m.Op == "work" {
		a.model(composeTokens) // put the answer into words
	}
	reply.Done, reply.Turn = m.Order, a.turn(session, true)
	return reply
}

// turn is what the agent remembers of a session: how far along it is.
func (a *plainAgent) turn(session string, advance bool) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if advance {
		a.turns[session]++
	}
	return a.turns[session]
}

// act carries out one order — after the agent's own check of whether it may.
func (a *plainAgent) act(session, order string) error {
	if strings.HasPrefix(order, forbiddenPrefix) {
		return errors.New("this agent does not make payments")
	}
	if err := a.callService(order); err != nil {
		return err
	}
	a.mu.Lock()
	a.seq++
	line, _ := json.Marshal(map[string]any{"seq": a.seq, "ts": time.Now().UnixMilli(),
		"session": session, "order": order, "outcome": "carried out"})
	_, err := a.log.Write(append(line, '\n'))
	a.mu.Unlock()
	return err
}

func (a *plainAgent) callService(order string) error {
	body, _ := json.Marshal(map[string]string{"id": order})
	req, _ := http.NewRequest(http.MethodPost, a.cfg.ServiceURL+"/orders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.ServiceKey)
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("the service answered %s", resp.Status)
	}
	return nil
}

// model asks the engine for n tokens and reads them all.
func (a *plainAgent) model(n int) {
	if a.cfg.EngineURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"model": "simulated", "stream": true, "max_tokens": n,
		"messages": []map[string]string{{"role": "user", "content": "plain"}}})
	req, _ := http.NewRequest(http.MethodPost, a.cfg.EngineURL+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.EngineKey)
	resp, err := a.http.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// check is the agent verifying its own audit log: every line a record, the
// numbers in order, nothing missing. It is a reasonable check, and it is all a
// plain file allows — the file does not know what it said yesterday.
func (a *plainAgent) check(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"ok": true, "entries": 0}
	f, err := os.Open(a.cfg.LogPath)
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		n := 0
		for sc.Scan() {
			n++
			var rec struct {
				Seq     int    `json:"seq"`
				Order   string `json:"order"`
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Seq != n || rec.Order == "" || rec.Outcome == "" {
				out["ok"], out["bad_line"] = false, n
				break
			}
		}
		out["entries"] = n
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// takenOver stands in for an attacker who has code execution in this process —
// the same position the demo takes against the node, where it acts as the
// Expertise that talks to the model, holding that Expertise's token.
//
// It is given nothing. Everything it uses it finds here: the key is in this
// process's configuration because the agent needs it to work, and the service
// cannot tell who is holding it. The agent's own rule in act is simply not on
// the path any more.
func (a *plainAgent) takenOver(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Orders []string `json:"orders"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	carried := 0
	for _, order := range body.Orders {
		if a.callService(order) == nil {
			carried++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"carried_out": carried})
}
