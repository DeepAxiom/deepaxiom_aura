package grammar

// The compiled-schema cache.
//
// Compiling a schema is cheap but not free, and the same handful of `std/*`
// refs are asked for on every skill registration and every validated delivery.
// This keeps one compiled copy per ref for the life of the process.

import (
	"fmt"
	"sort"
	"sync"

	"aura/kernel/internal/spec"
)

// Registry resolves a C1 schema reference to its parsed schema and compiled
// grammar. Safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	schemas  map[string]*Schema
	grammars map[string]string
	// sources is the raw document per ref, so a caller that wants the schema
	// itself (the UI, a client generating its own grammar) gets exactly what
	// the compiler saw.
	sources map[string]string
}

// NewRegistry builds a registry over the `std` namespace embedded at build
// time from spec/schemas/std (see spec.StdSchemas, generated).
func NewRegistry() *Registry {
	r := &Registry{
		schemas:  map[string]*Schema{},
		grammars: map[string]string{},
		sources:  map[string]string{},
	}
	for ref, raw := range spec.StdSchemas {
		r.sources[ref] = raw
	}
	return r
}

// Register adds or replaces a schema outside the `std` namespace — a skill's
// own `acme/thing@1`, once a mechanism exists for skills to publish one.
// Present now so the rest of the code never has to special-case `std`.
func (r *Registry) Register(ref, document string) error {
	if _, err := Parse([]byte(document)); err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources[ref] = document
	delete(r.schemas, ref)
	delete(r.grammars, ref)
	return nil
}

// Source returns the raw schema document for a ref.
func (r *Registry) Source(ref string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	doc, ok := r.sources[ref]
	return doc, ok
}

// Schema returns the parsed schema for a ref, parsing on first use.
func (r *Registry) Schema(ref string) (*Schema, bool) {
	r.mu.RLock()
	s, ok := r.schemas[ref]
	r.mu.RUnlock()
	if ok {
		return s, true
	}

	doc, ok := r.Source(ref)
	if !ok {
		return nil, false
	}
	parsed, err := Parse([]byte(doc))
	if err != nil {
		// Unreachable for `std` (the SSOT check parses them) and for anything
		// Register accepted, since that parses first.
		return nil, false
	}
	r.mu.Lock()
	r.schemas[ref] = parsed
	r.mu.Unlock()
	return parsed, true
}

// Grammar returns the GBNF grammar for a ref, compiling on first use.
func (r *Registry) Grammar(ref string) (string, error) {
	r.mu.RLock()
	g, ok := r.grammars[ref]
	r.mu.RUnlock()
	if ok {
		return g, nil
	}

	s, ok := r.Schema(ref)
	if !ok {
		return "", fmt.Errorf("no schema registered for %q", ref)
	}
	g, err := Compile(s)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	r.mu.Lock()
	r.grammars[ref] = g
	r.mu.Unlock()
	return g, nil
}

// Validate checks a payload against the schema for ref.
//
// An unknown ref is NOT a validation failure: a skill may legitimately declare
// a schema this kernel has never seen, and refusing its traffic would make
// every custom schema a breaking change. The caller learns the difference from
// `known`.
func (r *Registry) Validate(ref string, payload []byte) (problems []Problem, known bool) {
	s, ok := r.Schema(ref)
	if !ok {
		return nil, false
	}
	return Validate(s, payload), true
}

// Refs lists every schema this registry knows, sorted.
func (r *Registry) Refs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.sources))
	for ref := range r.sources {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}
