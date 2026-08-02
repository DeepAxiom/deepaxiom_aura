package projection

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"aura/kernel/internal/channel"
)

// Host runs one projection: every enabled operation connects to the kernel
// as an individual skill over the standard WS protocol (no privileges).
type Host struct {
	cfg      Config
	kernelWS string
	token    string
	log      *slog.Logger
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	client   *http.Client
}

// Manager owns all running projection hosts on this node.
type Manager struct {
	mu       sync.Mutex
	hosts    map[string]*Host
	kernelWS string
	// Token authenticates the host's connections back into its own node.
	// A projection is an ordinary skill client with no privileges — that is
	// the whole point of the design — so it authenticates like one.
	Token string
	Log   *slog.Logger
}

func NewManager(kernelWS string, log *slog.Logger) *Manager {
	return &Manager{hosts: map[string]*Host{}, kernelWS: kernelWS, Log: log}
}

// Apply (re)starts the host for a projection config.
func (m *Manager) Apply(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.hosts[cfg.Name]; ok {
		h.stop()
	}
	h := &Host{
		cfg: cfg, kernelWS: m.kernelWS, token: m.Token, log: m.Log,
		client: &http.Client{Timeout: 30 * time.Second},
	}
	m.hosts[cfg.Name] = h
	h.start()
}

func (h *Host) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	for _, op := range h.cfg.Ops {
		if op.Mode == ModeDisabled {
			continue
		}
		h.wg.Add(1)
		go h.serveOp(ctx, op)
	}
}

func (h *Host) stop() {
	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()
}

func (h *Host) manifest(op Op) map[string]any {
	skillType, capType := "sensorial", "sensorial"
	if op.Write {
		skillType, capType = "motor", "motor"
	}
	desc := fmt.Sprintf("%s (%s %s, api %q", op.Summary, op.Method, op.Path, h.cfg.Name)
	if op.Mode == ModeDryRun {
		desc += ", DRY-RUN: does not execute, returns what it would do"
	}
	desc += ")"
	var params []string
	for _, p := range op.Params {
		params = append(params, p.In+":"+p.Name)
	}
	if len(params) > 0 {
		// Planners parse this trailing "parameters:" list — keep the format
		// in sync with the planner skill's _declared_params().
		desc += " parameters: " + strings.Join(params, ", ")
	}
	host := ""
	if u, err := url.Parse(h.cfg.BaseURL); err == nil {
		host = u.Host
	}
	return map[string]any{
		"id":          fmt.Sprintf("projection/%s/%s", h.cfg.Name, op.OpID),
		"version":     "1.0.0",
		"protocol":    "1",
		"name":        fmt.Sprintf("%s · %s", h.cfg.Name, op.OpID),
		"description": desc,
		"capability":  fmt.Sprintf("%s.api.%s.%s", capType, slug(h.cfg.Name), strings.ReplaceAll(op.OpID, "-", "_")),
		"type":        skillType,
		"format":      "projection",
		"ports": map[string]any{
			"ingress": []map[string]string{{"name": "request_in", "schema": "std/api-request@1"}},
			"egress": []map[string]string{
				{"name": "response_out", "schema": "std/api-response@1"},
				{"name": "status_out", "schema": "std/status@1"},
			},
		},
		"permissions": map[string]any{
			"egress_http": []string{host},
			"filesystem":  "none",
			"channels":    "declared-only",
		},
	}
}

// serveOp keeps one operation registered as a live skill, reconnecting forever.
func (h *Host) serveOp(ctx context.Context, op Op) {
	defer h.wg.Done()
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := h.session(ctx, op); err != nil && ctx.Err() == nil {
			h.log.Debug("projection op reconnecting", "op", op.OpID, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}

func (h *Host) session(ctx context.Context, op Op) error {
	// A projection dials its own node like any other skill, so it presents the
	// node token like any other skill. The header form is used rather than the
	// query form because this is not a browser and a credential in a URL ends
	// up in logs.
	var headers http.Header
	if h.token != "" {
		headers = http.Header{"Authorization": []string{"Bearer " + h.token}}
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, h.kernelWS, headers)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()

	reg := map[string]any{"v": "1", "id": channel.NewID(), "kind": "register",
		"payload": h.manifest(op)}
	if err := conn.WriteJSON(reg); err != nil {
		return err
	}
	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		return err
	}
	if ack["kind"] == "error" {
		return fmt.Errorf("kernel rejected projection manifest: %v", ack["payload"])
	}

	var writeMu sync.Mutex
	seq := 0
	for {
		var env map[string]any
		if err := conn.ReadJSON(&env); err != nil {
			return err
		}
		if env["kind"] != "data" {
			continue
		}
		seq++
		result := h.execute(op, env["payload"])
		payload, _ := json.Marshal(result)
		reply := map[string]any{
			"v": "1", "id": channel.NewID(), "cause_id": env["id"],
			"session": env["session"], "node": env["node"],
			"port": "response_out", "seq": seq,
			"idem":   fmt.Sprintf("%v:%v:response_out:%d", env["idem"], env["node"], seq),
			"schema": "std/api-response@1", "kind": "data",
			"payload": json.RawMessage(payload),
		}
		writeMu.Lock()
		err := conn.WriteJSON(reply)
		writeMu.Unlock()
		if err != nil {
			return err
		}
	}
}

// execute performs (or dry-runs) the HTTP call described by the request payload.
// Request payload (std/api-request@1):
//
//	{ "params": {...path params...}, "query": {...}, "headers": {...}, "body": any }
func (h *Host) execute(op Op, rawPayload any) map[string]any {
	var req struct {
		Params  map[string]any `json:"params"`
		Query   map[string]any `json:"query"`
		Headers map[string]any `json:"headers"`
		Body    any            `json:"body"`
	}
	if b, err := json.Marshal(rawPayload); err == nil {
		_ = json.Unmarshal(b, &req)
	}

	// Build URL: substitute {param} then append query.
	path := op.Path
	for k, v := range req.Params {
		path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(fmt.Sprint(v)))
	}
	full := h.cfg.BaseURL + path
	if len(req.Query) > 0 {
		q := url.Values{}
		for k, v := range req.Query {
			q.Set(k, fmt.Sprint(v))
		}
		full += "?" + q.Encode()
	}

	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = json.Marshal(req.Body)
	}

	if op.Mode == ModeDryRun {
		return map[string]any{
			"ok": true, "dry_run": true, "status": 0,
			"request": map[string]any{
				"method": op.Method, "url": full,
				"body": json.RawMessage(orNull(bodyBytes)),
			},
		}
	}

	httpReq, err := http.NewRequest(op.Method, full, bytes.NewReader(bodyBytes))
	if err != nil {
		return map[string]any{"ok": false, "status": 0, "error": err.Error()}
	}
	if bodyBytes != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range h.cfg.Headers {
		httpReq.Header.Set(k, v)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, fmt.Sprint(v))
	}

	resp, err := h.client.Do(httpReq)
	if err != nil {
		return map[string]any{"ok": false, "status": 0, "error": err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	var parsed any
	if json.Unmarshal(data, &parsed) != nil {
		parsed = string(data)
	}
	return map[string]any{
		"ok": resp.StatusCode < 400, "status": resp.StatusCode, "body": parsed,
	}
}

func orNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
