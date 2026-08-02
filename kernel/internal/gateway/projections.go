package gateway

import (
	"encoding/json"
	"net/http"
	"sort"

	"aura/kernel/internal/projection"
)

// Projection endpoints (legacy-first):
//
//	POST /v1/projections                   introspect a spec, persist, start
//	GET  /v1/projections                   list projections + operation modes
//	POST /v1/projections/{name}/promote    change one operation's mode
//
// The projection host connects back into the kernel as ordinary skills.

type connectRequest struct {
	Name    string            `json:"name"`
	BaseURL string            `json:"base_url"`
	Headers map[string]string `json:"headers"`
	Kind    string            `json:"kind"` // only "openapi" today
	Spec    string            `json:"spec"` // raw document (JSON or YAML)
}

func (g *Gateway) connectProjection(w http.ResponseWriter, r *http.Request) {
	var req connectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if req.Kind != "" && req.Kind != "openapi" {
		writeJSON(w, 422, map[string]string{"error": "unsupported projection kind " + req.Kind})
		return
	}
	cfg, err := projection.ParseOpenAPI(req.Name, req.BaseURL, req.Headers, []byte(req.Spec))
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	if err := g.saveAndApply(cfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, cfg)
}

func (g *Gateway) listProjections(w http.ResponseWriter, _ *http.Request) {
	raw, err := g.St.LoadProjections()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := []projection.Config{}
	for name, cfg := range raw {
		var c projection.Config
		if err := json.Unmarshal(cfg, &c); err != nil {
			g.Log.Error("stored projection is corrupt", "name", name, "err", err)
			continue
		}
		// A projection's headers are its credentials — an API key, a bearer
		// token. They are write-only: needed to call the target system, never
		// returned to a client. Listing them here would hand every reader of
		// the control-plane UI the keys to the system being projected. The
		// names are kept so an operator can see that auth is configured.
		c.Headers = redactHeaders(c.Headers)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, map[string]any{"projections": out})
}

// redactHeaders keeps each header name but replaces its value.
func redactHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers))
	for name := range headers {
		out[name] = "«redacted»"
	}
	return out
}

func (g *Gateway) promoteOperation(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Op   string `json:"op"`
		Mode string `json:"mode"` // live | dry-run | disabled
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if req.Mode != projection.ModeLive && req.Mode != projection.ModeDryRun &&
		req.Mode != projection.ModeDisabled {
		writeJSON(w, 422, map[string]string{"error": "mode must be live|dry-run|disabled"})
		return
	}

	all, err := g.St.LoadProjections()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	raw, ok := all[name]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "projection not found: " + name})
		return
	}
	var cfg projection.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	found := false
	for i := range cfg.Ops {
		if cfg.Ops[i].OpID == req.Op {
			cfg.Ops[i].Mode = req.Mode
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, 404, map[string]string{"error": "operation not found: " + req.Op})
		return
	}
	if err := g.saveAndApply(&cfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, cfg)
}

func (g *Gateway) saveAndApply(cfg *projection.Config) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := g.St.SaveProjection(cfg.Name, b); err != nil {
		return err
	}
	g.Proj.Apply(*cfg)
	g.Log.Info("projection applied", "name", cfg.Name, "ops", len(cfg.Ops))
	return nil
}

// StartSavedProjections boots every persisted projection (called at startup).
func (g *Gateway) StartSavedProjections() {
	all, err := g.St.LoadProjections()
	if err != nil {
		g.Log.Error("load projections failed", "err", err)
		return
	}
	for name, raw := range all {
		var cfg projection.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			g.Log.Error("bad projection config", "name", name, "err", err)
			continue
		}
		g.Proj.Apply(cfg)
	}
	if len(all) > 0 {
		g.Log.Info("projections restored", "count", len(all))
	}
}
