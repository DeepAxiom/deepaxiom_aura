// Package registry implements kernel primitive P3 — the live capability registry.
//
// Skills connect INTO the kernel (IoC/outbound: NAT- and firewall-friendly),
// push their C1 manifest, and become resolvable by capability.
package registry

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"aura/kernel/internal/spec"
)

// Port is a typed port declaration (C1).
type Port struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
}

// ConfigParam declares one runtime-tunable parameter (C1, additive field).
// It is metadata only — what a value CAN be, not what it currently IS.
// Effective values live outside the (immutable) manifest; see
// Manifest.EffectiveConfig.
type ConfigParam struct {
	Key             string   `json:"key"`
	Type            string   `json:"type"` // string | int | float | bool | enum
	Default         any      `json:"default"`
	Min             *float64 `json:"min,omitempty"`
	Max             *float64 `json:"max,omitempty"`
	Options         []string `json:"options,omitempty"` // for type=enum
	Description     string   `json:"description,omitempty"`
	RestartRequired bool     `json:"restart_required,omitempty"` // e.g. context window size
}

// Compensation declares how to undo one motor skill's effect (C1, additive,
// Phase 2 groundwork). `port` names an ingress port the skill also declares in
// `ports.ingress`; `aura undo` (Phase 2) sends the original effect's payload
// back on it. Declaring this is optional and changes nothing about how the
// skill runs today — the ledger records it so a future `aura undo` has
// something to call, and so a session's receipt can say whether an effect was
// reversible at all.
type Compensation struct {
	Port   string `json:"port"`
	Schema string `json:"schema"`
}

// Manifest is contract C1. Field-level validation mirrors manifest.schema.json.
type Manifest struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Protocol    string `json:"protocol"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Capability  string `json:"capability"`
	Type        string `json:"type"`
	Format      string `json:"format"`
	Ports       struct {
		Ingress []Port `json:"ingress"`
		Egress  []Port `json:"egress"`
	} `json:"ports"`
	Requirements map[string]any `json:"requirements,omitempty"`
	Permissions  map[string]any `json:"permissions,omitempty"`
	Config       []ConfigParam  `json:"config,omitempty"`
	Compensates  *Compensation  `json:"compensates,omitempty"`
}

var (
	idRe     = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9-]+/[a-z0-9-]+$`)
	semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	capRe    = regexp.MustCompile(spec.CapabilityPattern)
	schemaRe = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9_-]+@\d+$`)
	portRe   = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// SkillTypes are the five kinds of skill (generated from spec/enums.yaml).
var SkillTypes = spec.SkillTypes

func (m *Manifest) Validate() error {
	switch {
	case !idRe.MatchString(m.ID):
		return fmt.Errorf("invalid id %q (want org/category/name, lowercase)", m.ID)
	case !semverRe.MatchString(m.Version):
		return fmt.Errorf("invalid version %q (want semver)", m.Version)
	case m.Protocol == "":
		return fmt.Errorf("missing protocol major")
	case m.Name == "" || m.Description == "":
		return fmt.Errorf("name and description are mandatory (the planner reads them)")
	case !capRe.MatchString(m.Capability):
		return fmt.Errorf("invalid capability %q (want <type>.<function>...)", m.Capability)
	case m.Format == "":
		return fmt.Errorf("missing format (source|wasm|model|projection)")
	}
	typeOK := false
	for _, t := range SkillTypes {
		if m.Type == t {
			typeOK = true
			break
		}
	}
	if !typeOK {
		return fmt.Errorf("invalid type %q (want one of %s)", m.Type, strings.Join(SkillTypes, "|"))
	}
	if !strings.HasPrefix(m.Capability, m.Type+".") {
		return fmt.Errorf("capability %q must start with type %q", m.Capability, m.Type)
	}
	if len(m.Ports.Ingress) == 0 && len(m.Ports.Egress) == 0 {
		return fmt.Errorf("skill declares no ports")
	}
	for _, p := range append(append([]Port{}, m.Ports.Ingress...), m.Ports.Egress...) {
		if !portRe.MatchString(p.Name) {
			return fmt.Errorf("invalid port name %q", p.Name)
		}
		if !schemaRe.MatchString(p.Schema) {
			return fmt.Errorf("port %q: schema is mandatory and must be ns/name@major, got %q", p.Name, p.Schema)
		}
	}
	if m.Compensates != nil {
		if m.Type != spec.EffectType {
			return fmt.Errorf("compensates is declared but type is %q; only %q skills act on the world",
				m.Type, spec.EffectType)
		}
		sch, ok := m.IngressSchema(m.Compensates.Port)
		if !ok {
			return fmt.Errorf("compensates.port %q is not a declared ingress port", m.Compensates.Port)
		}
		if m.Compensates.Schema != "" && m.Compensates.Schema != sch {
			return fmt.Errorf("compensates.schema %q does not match ingress port %q's declared schema %q",
				m.Compensates.Schema, m.Compensates.Port, sch)
		}
	}
	return nil
}

// ConfigParam returns the declared spec for one config key, if any.
func (m *Manifest) ConfigParam(key string) (ConfigParam, bool) {
	for _, p := range m.Config {
		if p.Key == key {
			return p, true
		}
	}
	return ConfigParam{}, false
}

// ValidateConfigValue checks one incoming value against its declared C1
// config param (type, range, enum membership). Unknown keys are rejected —
// config is capability-based like permissions: nothing undeclared is
// accepted.
func (m *Manifest) ValidateConfigValue(key string, value any) error {
	p, ok := m.ConfigParam(key)
	if !ok {
		return fmt.Errorf("skill %q declares no config key %q", m.ID, key)
	}
	switch p.Type {
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: want string, got %T", key, value)
		}
	case "bool":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: want bool, got %T", key, value)
		}
	case "int", "float":
		n, ok := value.(float64) // JSON numbers decode as float64
		if !ok {
			return fmt.Errorf("%s: want number, got %T", key, value)
		}
		if p.Type == "int" && n != float64(int64(n)) {
			return fmt.Errorf("%s: want an integer, got %v", key, n)
		}
		if p.Min != nil && n < *p.Min {
			return fmt.Errorf("%s: %v is below min %v", key, n, *p.Min)
		}
		if p.Max != nil && n > *p.Max {
			return fmt.Errorf("%s: %v is above max %v", key, n, *p.Max)
		}
	case "enum":
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s: want string (one of %v), got %T", key, p.Options, value)
		}
		valid := false
		for _, o := range p.Options {
			if o == s {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("%s: %q is not one of %v", key, s, p.Options)
		}
	default:
		return fmt.Errorf("%s: skill declares unknown config type %q", key, p.Type)
	}
	return nil
}

// EffectiveConfig merges declared defaults with the file layer (lowest
// priority override, reproducible via git) and the store layer (highest
// priority, set live via the UI/API) into the values a skill should run
// with. Precedence: store override > config file > manifest default.
func (m *Manifest) EffectiveConfig(fileVals, storeVals map[string]any) map[string]any {
	out := make(map[string]any, len(m.Config))
	for _, p := range m.Config {
		out[p.Key] = p.Default
	}
	for k, v := range fileVals {
		if _, declared := m.ConfigParam(k); declared {
			out[k] = v
		}
	}
	for k, v := range storeVals {
		if _, declared := m.ConfigParam(k); declared {
			out[k] = v
		}
	}
	return out
}

// EgressSchema returns the declared schema of an egress port, if any.
func (m *Manifest) EgressSchema(port string) (string, bool) {
	for _, p := range m.Ports.Egress {
		if p.Name == port {
			return p.Schema, true
		}
	}
	return "", false
}

// IngressSchema returns the declared schema of an ingress port, if any.
func (m *Manifest) IngressSchema(port string) (string, bool) {
	for _, p := range m.Ports.Ingress {
		if p.Name == port {
			return p.Schema, true
		}
	}
	return "", false
}

// Live is a connected skill: manifest + a serialized way to reach it.
type Live struct {
	Manifest  Manifest
	Connected time.Time
	// Send writes to the skill's transport, serialized. The qos argument is
	// the declared class of the edge the message travels on (C3 rule 4): it
	// decides whether a slow receiver blocks the producer or loses frames.
	Send func(raw []byte, qos string) error
}

// Registry is the in-memory live registry (persistence is the store's job).
type Registry struct {
	mu     sync.RWMutex
	byConn map[string]*Live // key: connection id
}

func New() *Registry {
	return &Registry{byConn: make(map[string]*Live)}
}

func (r *Registry) Register(connID string, live *Live) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byConn[connID] = live
}

func (r *Registry) Unregister(connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byConn, connID)
}

// Resolve satisfies a demand: exact package id ("use") or capability
// ("resolve", exact or prefix). Deterministic: newest connection wins ties.
func (r *Registry) Resolve(use, capability string) (*Live, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var candidates []*Live
	for _, l := range r.byConn {
		if use != "" && l.Manifest.ID == use {
			candidates = append(candidates, l)
		}
		if use == "" && capability != "" &&
			(l.Manifest.Capability == capability || strings.HasPrefix(l.Manifest.Capability, capability+".")) {
			candidates = append(candidates, l)
		}
	}
	if len(candidates) == 0 {
		if use != "" {
			return nil, fmt.Errorf("no connected skill provides package %q", use)
		}
		return nil, fmt.Errorf("no connected skill provides capability %q", capability)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Connected.After(candidates[j].Connected)
	})
	return candidates[0], nil
}

// ResolvePreferred is Resolve with the node's routing preferences applied
// (C4 policy `routes`).
//
// Preferences are advisory by construction: a preferred package that is not
// connected falls through to ordinary resolution rather than failing the
// session. Routing decides *which* of several equivalent providers answers,
// and a graph should not go down because the operator's first choice is
// restarting. Forbidding a provider outright is what a `deny` rule is for.
func (r *Registry) ResolvePreferred(use, capability string, prefer []string, avoid map[string]bool) (*Live, error) {
	if use != "" || capability == "" || (len(prefer) == 0 && len(avoid) == 0) {
		return r.Resolve(use, capability)
	}

	r.mu.RLock()
	var candidates []*Live
	for _, l := range r.byConn {
		if l.Manifest.Capability == capability ||
			strings.HasPrefix(l.Manifest.Capability, capability+".") {
			candidates = append(candidates, l)
		}
	}
	r.mu.RUnlock()
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no connected skill provides capability %q", capability)
	}

	for _, want := range prefer {
		for _, l := range candidates {
			if l.Manifest.ID == want {
				return l, nil
			}
		}
	}

	if len(avoid) > 0 {
		var kept []*Live
		for _, l := range candidates {
			if !avoid[l.Manifest.ID] {
				kept = append(kept, l)
			}
		}
		// Only honour `avoid` when something is left: an avoided provider that
		// is the sole provider is still better than no provider.
		if len(kept) > 0 {
			candidates = kept
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Connected.After(candidates[j].Connected)
	})
	return candidates[0], nil
}

// SendToID pushes raw to every live connection of skill id (usually zero
// or one, but nothing stops two instances of the same skill connecting).
// Used to push a config_update after a runtime override changes.
func (r *Registry) SendToID(id string, raw []byte) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sent := 0
	for _, l := range r.byConn {
		// A config change is control traffic: it must arrive, so it never
		// travels on a lossy lane whatever the skill's edges declare.
		if l.Manifest.ID == id && l.Send(raw, "reliable") == nil {
			sent++
		}
	}
	return sent
}

// Catalog returns all live manifests (what a planner reads).
func (r *Registry) Catalog() []Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Manifest, 0, len(r.byConn))
	for _, l := range r.byConn {
		out = append(out, l.Manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CatalogJSON is Catalog marshalled once, for handlers.
func (r *Registry) CatalogJSON() []byte {
	b, _ := json.Marshal(r.Catalog())
	return b
}
