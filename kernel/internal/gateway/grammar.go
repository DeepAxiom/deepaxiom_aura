package gateway

// The typed-port surface: schemas and the decoding grammars derived from them.
//
// C1 makes a schema mandatory on every port and C2 rule 2 refuses to wire two
// ports whose schemas disagree, so the runtime already knows the exact shape
// of everything a skill may emit. This exposes that knowledge in the one form
// a model can be held to: a grammar.
//
// The consequence is the strongest type guarantee in the system. A skill that
// generates under its port's grammar cannot produce a schema violation —
// not "rarely does", cannot — and anything that does not generate is checked
// against the same schema on the way past (see grammar.Validate).

import (
	"net/http"

	"aura/kernel/internal/grammar"
	"aura/kernel/internal/registry"
)

// egressGrammars compiles a grammar for every distinct schema this skill's
// egress ports declare, keyed by port name.
//
// Keyed by port rather than by schema because that is how a skill thinks: it
// is about to emit on `text_out` and wants the grammar for `text_out`, not to
// look up which schema that port declared and then find the grammar for it.
//
// A schema this kernel does not know is silently absent rather than an error.
// A skill may legitimately declare `acme/invoice@1`, and refusing its
// registration over a grammar it never asked for would make every custom
// schema a breaking change.
func (g *Gateway) egressGrammars(m registry.Manifest) map[string]string {
	if g.Grammars == nil {
		return nil
	}
	out := map[string]string{}
	for _, port := range m.Ports.Egress {
		if port.Schema == "" {
			continue
		}
		gbnf, err := g.Grammars.Grammar(port.Schema)
		if err != nil {
			g.Log.Debug("no grammar for port schema",
				"skill", m.ID, "port", port.Name, "schema", port.Schema, "err", err)
			continue
		}
		out[port.Name] = gbnf
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// listSchemas returns every schema reference this node can compile
// (GET /v1/schemas).
func (g *Gateway) listSchemas(w http.ResponseWriter, _ *http.Request) {
	if g.Grammars == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no schema registry"})
		return
	}
	writeJSON(w, 200, map[string]any{"schemas": g.Grammars.Refs()})
}

// getSchema returns one schema document (GET /v1/schemas/{ref}).
//
// The ref carries a slash (`std/text@1`), so it arrives as a wildcard path
// segment rather than a single {ref} — see the route registration.
func (g *Gateway) getSchema(w http.ResponseWriter, r *http.Request) {
	if g.Grammars == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no schema registry"})
		return
	}
	ref := r.PathValue("ref")
	doc, ok := g.Grammars.Source(ref)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no schema registered for " + ref})
		return
	}
	w.Header().Set("Content-Type", "application/schema+json")
	_, _ = w.Write([]byte(doc))
}

// getGrammar returns the decoding grammar for one schema
// (GET /v1/grammars/{ref}).
//
// A sibling path rather than a suffix on the schema route, because a schema
// ref contains slashes and Go's ServeMux only allows a multi-segment wildcard
// at the end of a pattern.
//
// Served as text/plain GBNF because that is what a decoder consumes: a caller
// pipes it straight into llama.cpp's `--grammar-file` or the equivalent
// binding, with no unwrapping step.
func (g *Gateway) getGrammar(w http.ResponseWriter, r *http.Request) {
	if g.Grammars == nil {
		writeJSON(w, 404, map[string]string{"error": "this node has no schema registry"})
		return
	}
	ref := r.PathValue("ref")
	gbnf, err := g.Grammars.Grammar(ref)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(gbnf))
}

// validatePayload checks a delivery against its port's declared schema.
//
// This is the enforcement half of the typed channel. The grammar makes a
// violation unreachable for a model that decodes under it; this makes one
// rejected for everything else — a skill building a dict by hand, a projected
// API returning a surprise, a client posting whatever it likes.
//
// Returns nil when the schema is unknown to this node: see egressGrammars for
// why an unrecognised ref must not be treated as a failure.
func (g *Gateway) validatePayload(schema string, payload []byte) []grammar.Problem {
	if g.Grammars == nil || schema == "" || len(payload) == 0 {
		return nil
	}
	problems, known := g.Grammars.Validate(schema, payload)
	if !known {
		return nil
	}
	return problems
}
