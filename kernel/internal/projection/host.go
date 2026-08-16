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

// Secrets is the credential broker as a projection needs it: exchange the
// receipt of a sealed effect for the values behind the ${secret:…} references
// in a header set.
//
// An interface rather than the concrete *broker.Broker so this package keeps
// importing nothing that knows about ledgers, and so a test can substitute one
// without a store, a keypair and a chain of sealed effects.
type Secrets interface {
	ResolveIn(receipt, capability string, in map[string]string) (map[string]string, error)
}

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
	secrets  Secrets
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
	// Secrets resolves ${secret:…} references against a sealed receipt. Nil on
	// a node with no broker, which makes any operation that references a secret
	// fail loudly rather than call the target system with the literal text
	// "${secret:erp_token}" in an Authorization header.
	Secrets Secrets
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
		client:  &http.Client{Timeout: 30 * time.Second},
		secrets: m.Secrets,
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
		// The receipt is the kernel's own proof that this delivery passed the
		// Effect Checkpoint. It is only ever set on an envelope the executor
		// sealed, so a projection cannot manufacture one for itself.
		receipt, _ := env["receipt"].(string)
		result := h.execute(op, env["payload"], receipt)
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
func (h *Host) execute(op Op, rawPayload any, receipt string) map[string]any {
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

	// Credentials are resolved here and nowhere earlier. The stored config
	// carries `${secret:name}`, not the value, so a projection sitting idle
	// holds nothing worth stealing and `GET /v1/projections` has nothing to
	// leak; the value exists only inside this function, only for this call,
	// and only because the receipt above proved the call was authorized.
	headers, err := h.resolveHeaders(op, receipt)
	if err != nil {
		return map[string]any{"ok": false, "status": 0, "error": err.Error()}
	}

	httpReq, err := http.NewRequest(op.Method, full, bytes.NewReader(bodyBytes))
	if err != nil {
		return map[string]any{"ok": false, "status": 0, "error": err.Error()}
	}
	if bodyBytes != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
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

// capability is the C1 capability this operation registers under. Kept next to
// manifest(), which is the other place the same string is built: the broker
// binds a receipt to the capability that earned it, so the two spellings
// diverging would mean every credentialed write silently failing.
func (h *Host) capability(op Op) string {
	capType := "sensorial"
	if op.Write {
		capType = "motor"
	}
	return fmt.Sprintf("%s.api.%s.%s", capType, slug(h.cfg.Name), strings.ReplaceAll(op.OpID, "-", "_"))
}

// resolveHeaders turns the configured headers into the ones actually sent,
// exchanging the receipt for any ${secret:…} they reference.
//
// A projection with no secrets is unaffected — no receipt is asked for and no
// broker is needed — which keeps this entirely opt-in for the OpenAPI target
// that authenticates with nothing, or with a header an operator is content to
// have sitting in the config.
func (h *Host) resolveHeaders(op Op, receipt string) (map[string]string, error) {
	if !brokerNeeded(h.cfg.Headers) {
		return h.cfg.Headers, nil
	}
	if h.secrets == nil {
		return nil, fmt.Errorf("operation %q references a stored secret but this node has no "+
			"credential broker configured", op.OpID)
	}
	// The failure is deliberately total rather than "send the call without the
	// header". A request that reaches the target system unauthenticated is a
	// request the target will answer — with a 401 if you are lucky, and with
	// unauthenticated-but-permitted data if you are not.
	out, err := h.secrets.ResolveIn(receipt, h.capability(op), h.cfg.Headers)
	if err != nil {
		return nil, fmt.Errorf("operation %q could not obtain its credential: %w", op.OpID, err)
	}
	return out, nil
}

// brokerNeeded reports whether any header references a stored secret. Kept
// local so this package needs no import of broker — the reference syntax is
// simple enough that duplicating the recognition is cheaper than the coupling.
func brokerNeeded(headers map[string]string) bool {
	for _, v := range headers {
		if strings.Contains(v, "${secret:") {
			return true
		}
	}
	return false
}

func orNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
