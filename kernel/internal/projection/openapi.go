// Package projection implements legacy projections: existing
// systems introspected and exposed as skills, without touching their code.
//
// This is distro userland that ships in the binary for convenience — it
// talks to the kernel over the same WS skill protocol as any third-party
// skill, gaining no kernel privileges.
package projection

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode of a projected operation. Reads default to live; writes to disabled
// (read-only by default, explicit promotion per operation).
const (
	ModeLive     = "live"
	ModeDryRun   = "dry-run"
	ModeDisabled = "disabled"
)

type Param struct {
	Name     string `json:"name"`
	In       string `json:"in"` // path | query | header
	Required bool   `json:"required"`
}

type Op struct {
	OpID    string  `json:"op_id"` // slug, unique within the projection
	Method  string  `json:"method"`
	Path    string  `json:"path"`
	Summary string  `json:"summary"`
	Write   bool    `json:"write"`
	Mode    string  `json:"mode"`
	Params  []Param `json:"params,omitempty"`
	HasBody bool    `json:"has_body,omitempty"`
}

type Config struct {
	Name    string            `json:"name"`
	BaseURL string            `json:"base_url"`
	Headers map[string]string `json:"headers,omitempty"`
	Ops     []Op              `json:"ops"`
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug converts anything (camelCase, "CRM Viejo", /path/{id}) into id-safe
// kebab: dashes only on lower/digit→upper transitions, so acronyms survive.
func slug(s string) string {
	var b strings.Builder
	var prev rune
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			if prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9' {
				b.WriteByte('-')
			}
			b.WriteRune(r + 32)
		} else {
			b.WriteRune(r)
		}
		prev = r
	}
	out := nonSlug.ReplaceAllString(b.String(), "-")
	return strings.Trim(out, "-")
}

// ParseOpenAPI introspects an OpenAPI 3 document (JSON or YAML) into a
// projection config. Write operations start disabled.
func ParseOpenAPI(name, baseURL string, headers map[string]string, raw []byte) (*Config, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		if err2 := yaml.Unmarshal(raw, &doc); err2 != nil {
			return nil, fmt.Errorf("spec is neither valid JSON nor YAML: %v", err2)
		}
	}
	if doc["openapi"] == nil && doc["swagger"] == nil {
		return nil, fmt.Errorf("document has no openapi/swagger field")
	}

	if name == "" {
		if info, ok := doc["info"].(map[string]any); ok {
			name, _ = info["title"].(string)
		}
	}
	name = slug(name)
	if name == "" {
		return nil, fmt.Errorf("projection needs a name (use --name)")
	}

	if baseURL == "" {
		if servers, ok := doc["servers"].([]any); ok && len(servers) > 0 {
			if s0, ok := servers[0].(map[string]any); ok {
				baseURL, _ = s0["url"].(string)
			}
		}
	}
	if baseURL == "" {
		return nil, fmt.Errorf("no servers[0].url in spec — pass --base-url")
	}

	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		return nil, fmt.Errorf("spec has no paths")
	}

	cfg := &Config{Name: name, BaseURL: strings.TrimRight(baseURL, "/"), Headers: headers}
	seen := map[string]bool{}
	methods := []string{"get", "head", "post", "put", "patch", "delete"}

	pathKeys := make([]string, 0, len(paths))
	for p := range paths {
		pathKeys = append(pathKeys, p)
	}
	sort.Strings(pathKeys)

	for _, path := range pathKeys {
		item, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		shared := parseParams(item["parameters"])
		for _, method := range methods {
			opRaw, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			opID, _ := opRaw["operationId"].(string)
			if opID == "" {
				opID = method + "-" + path
			}
			opID = slug(opID)
			for i := 2; seen[opID]; i++ { // ensure uniqueness
				opID = fmt.Sprintf("%s-%d", opID, i)
			}
			seen[opID] = true

			summary, _ := opRaw["summary"].(string)
			if summary == "" {
				summary, _ = opRaw["description"].(string)
			}
			if summary == "" {
				summary = strings.ToUpper(method) + " " + path
			}
			write := method != "get" && method != "head"
			mode := ModeLive
			if write {
				mode = ModeDisabled
			}
			cfg.Ops = append(cfg.Ops, Op{
				OpID: opID, Method: strings.ToUpper(method), Path: path,
				Summary: summary, Write: write, Mode: mode,
				Params:  append(shared, parseParams(opRaw["parameters"])...),
				HasBody: opRaw["requestBody"] != nil,
			})
		}
	}
	if len(cfg.Ops) == 0 {
		return nil, fmt.Errorf("no operations found in spec")
	}
	return cfg, nil
}

func parseParams(raw any) []Param {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []Param
	for _, p := range list {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := pm["name"].(string)
		in, _ := pm["in"].(string)
		req, _ := pm["required"].(bool)
		if name != "" && (in == "path" || in == "query" || in == "header") {
			out = append(out, Param{Name: name, In: in, Required: req})
		}
	}
	return out
}
