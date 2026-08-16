// Package grammar compiles a port's declared JSON Schema into a decoding
// grammar, so a model physically cannot emit something the port would reject.
//
// # Why this can exist here and nowhere else
//
// Constrained decoding is not new — XGrammar, llguidance and Outlines all do
// it well, and every serving stack ships one. What is unusual is *where the
// grammar comes from*. Everywhere else it comes from a schema the application
// author wrote by hand and passed to the inference call, which means it is
// only as correct as their discipline and it drifts from whatever consumes the
// output.
//
// Here the grammar is derived from the **type of the port the output is going
// into**. C1 makes a schema mandatory on every port (rule: "every port MUST
// declare a schema"), and C2 rule 2 already refuses to wire two ports whose
// schemas disagree. So the runtime knows, statically, exactly what shape a
// skill's output has to be — and can hand the model a grammar that makes any
// other shape unreachable.
//
// The consequence is stronger than convenience: a graph that type-checks and a
// model that decodes under this grammar cannot produce a schema violation at
// all. Not "usually does not" — cannot. No other agent runtime is in a
// position to offer that, because none of them have mandatory typed ports.
//
// # Why the compiler is in the kernel and not the SDK
//
// It looks like it belongs in userland: grammars are for language models, and
// the kernel's purity test is "if a feature needs an LLM to work, it does not
// belong in the kernel". A schema-to-grammar compiler needs no LLM — it is a
// pure syntactic transform over a document the kernel already owns and already
// validates against. Keeping it here means every skill in every language gets
// the same grammar for the same port, rather than three SDKs each
// reimplementing JSON Schema subset handling slightly differently. That
// divergence is exactly the failure the SSOT discipline exists to prevent.
//
// # GBNF, and what it means for other engines
//
// The output is GBNF, llama.cpp's grammar format, because that is what the
// first-party model driver consumes. GBNF is also the most widely accepted
// interchange format for local constrained decoding. An engine that speaks
// something else (XGrammar's EBNF, an Outlines regex) needs a translation
// layer in its own skill; the schema is the contract, GBNF is one rendering
// of it, and `GET /v1/schemas/{ref}/grammar` serves whichever renderings this
// kernel knows.
package grammar

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Schema is the subset of JSON Schema this compiler understands.
//
// Deliberately a subset, and deliberately explicit about being one. The `std`
// namespace uses object/string/number/integer/boolean/array/enum and nothing
// else, so supporting more would be speculative work whose bugs nobody would
// find. Compile returns an error on anything outside it rather than silently
// emitting a grammar that permits more than the schema does — a permissive
// grammar is worse than no grammar, because it looks like a guarantee.
type Schema struct {
	Type        string             `json:"type"`
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Required    []string           `json:"required"`
	Properties  map[string]*Schema `json:"properties"`
	Items       *Schema            `json:"items"`
	Enum        []any              `json:"enum"`
	// AdditionalProperties is read but only honoured when explicitly false.
	// Absent means "anything else may follow", which a grammar cannot express
	// usefully — see compileObject.
	AdditionalProperties *bool `json:"additionalProperties"`

	Minimum *float64 `json:"minimum"`
	Maximum *float64 `json:"maximum"`

	// order is the sequence `properties` was written in. Go maps lose it, and
	// it matters: see propertyOrder.
	order []string
}

// Parse reads a JSON Schema document, preserving the order its properties
// were declared in.
func Parse(raw []byte) (*Schema, error) {
	var s Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("schema is not valid JSON: %w", err)
	}
	if err := s.recordOrder(raw); err != nil {
		return nil, err
	}
	return &s, nil
}

// recordOrder walks the raw document to recover declaration order, which
// json.Unmarshal into a map discards.
//
// Worth the extra pass because the order a grammar emits properties in is the
// order the model has to *decide* them in, and schema authors already put them
// in a sensible one. `std/transcript@1` declares `text` then `final`;
// alphabetical would make a model commit to `final` before it has written the
// text it is describing, which is backwards and measurably worse for the
// smaller local models this runtime targets.
func (s *Schema) recordOrder(raw []byte) error {
	if len(s.Properties) == 0 {
		return nil
	}
	var probe struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Properties) == 0 {
		return nil
	}
	keys, err := objectKeys(probe.Properties)
	if err != nil {
		return fmt.Errorf("reading property order: %w", err)
	}
	s.order = keys

	// Recurse so nested objects keep their order too.
	var subs map[string]json.RawMessage
	if err := json.Unmarshal(probe.Properties, &subs); err != nil {
		return nil
	}
	for key, sub := range subs {
		if child, ok := s.Properties[key]; ok && child != nil {
			if err := child.recordOrder(sub); err != nil {
				return err
			}
		}
	}
	if s.Items != nil {
		var itemsRaw struct {
			Items json.RawMessage `json:"items"`
		}
		if json.Unmarshal(raw, &itemsRaw) == nil && len(itemsRaw.Items) > 0 {
			if err := s.Items.recordOrder(itemsRaw.Items); err != nil {
				return err
			}
		}
	}
	return nil
}

// objectKeys returns a JSON object's keys in document order.
func objectKeys(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected an object")
	}
	var keys []string
	depth := 0
	for dec.More() || depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			continue
		}
		if depth == 0 {
			key, ok := tok.(string)
			if !ok {
				return nil, fmt.Errorf("expected a property name")
			}
			keys = append(keys, key)
			// Skip this property's value wholesale.
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return nil, err
			}
		}
	}
	return keys, nil
}

// propertyOrder returns declared order, falling back to sorted order for a
// schema built in Go rather than parsed (tests, future programmatic use).
func (s *Schema) propertyOrder() []string {
	if len(s.order) == len(s.Properties) {
		return s.order
	}
	out := make([]string, 0, len(s.Properties))
	for key := range s.Properties {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Compile renders a schema as a GBNF grammar whose root production is `root`.
//
// The grammar accepts exactly the JSON documents the schema accepts, for the
// supported subset, with two documented relaxations noted at their sites:
// numeric bounds and property ordering.
func Compile(s *Schema) (string, error) {
	c := &compiler{rules: map[string]string{}, order: []string{}}
	root, err := c.compile(s, "root")
	if err != nil {
		return "", err
	}
	if root != "root" {
		c.emit("root", root)
	}

	var b strings.Builder
	for _, name := range c.order {
		fmt.Fprintf(&b, "%s ::= %s\n", name, c.rules[name])
	}
	b.WriteString(primitives)
	return b.String(), nil
}

// CompileRef compiles one of the built-in `std/*` schemas by its C1 reference.
func CompileRef(ref string, lookup func(string) (string, bool)) (string, error) {
	raw, ok := lookup(ref)
	if !ok {
		return "", fmt.Errorf("no schema registered for %q", ref)
	}
	s, err := Parse([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	g, err := Compile(s)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	return g, nil
}

type compiler struct {
	rules map[string]string
	order []string
	n     int
}

func (c *compiler) emit(name, body string) {
	if _, exists := c.rules[name]; !exists {
		c.order = append(c.order, name)
	}
	c.rules[name] = body
}

func (c *compiler) fresh(hint string) string {
	c.n++
	return fmt.Sprintf("%s-%d", sanitize(hint), c.n)
}

// compile returns the name of a rule matching s.
func (c *compiler) compile(s *Schema, name string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("nil schema")
	}

	// An enum constrains harder than a type does, so it wins when both are
	// present — and it is the one case where a grammar is strictly stronger
	// than post-hoc validation, since an invalid value is unreachable rather
	// than rejected after the fact.
	if len(s.Enum) > 0 {
		alts := make([]string, 0, len(s.Enum))
		for _, v := range s.Enum {
			lit, err := json.Marshal(v)
			if err != nil {
				return "", fmt.Errorf("enum value %v is not JSON-representable", v)
			}
			alts = append(alts, quoteGBNF(string(lit)))
		}
		c.emit(name, strings.Join(alts, " | "))
		return name, nil
	}

	switch s.Type {
	case "object":
		return c.compileObject(s, name)
	case "array":
		return c.compileArray(s, name)
	case "string":
		c.emit(name, "string")
		return name, nil
	case "integer":
		c.emit(name, "integer")
		return name, nil
	case "number":
		c.emit(name, "number")
		return name, nil
	case "boolean":
		c.emit(name, "boolean")
		return name, nil
	case "null":
		c.emit(name, `"null"`)
		return name, nil
	case "":
		// An empty schema (`"body": {}` in std/api-response@1) means "any
		// JSON". Permitting anything here is correct rather than lax: the
		// schema itself imposes no constraint, so a grammar that imposed one
		// would reject documents the port accepts.
		c.emit(name, "value")
		return name, nil
	default:
		return "", fmt.Errorf(
			"unsupported schema type %q — this compiler covers "+
				"object/array/string/integer/number/boolean/null and enum, which is what "+
				"the std namespace uses; a grammar for an unhandled type would permit more "+
				"than the schema does", s.Type)
	}
}

func (c *compiler) compileObject(s *Schema, name string) (string, error) {
	if len(s.Properties) == 0 {
		// `{"type":"object"}` with no declared properties: any object.
		c.emit(name, "object")
		return name, nil
	}

	required := map[string]bool{}
	for _, r := range s.Required {
		if _, declared := s.Properties[r]; !declared {
			return "", fmt.Errorf("required property %q is not declared in properties", r)
		}
		required[r] = true
	}

	// Ordering is the one real concession this compiler makes: JSON objects
	// are unordered, but a grammar is a sequence, so a fixed order has to be
	// chosen. Required properties come first (so a grammar can attach commas
	// safely — see below), and within each group the schema's own declaration
	// order is preserved rather than sorted, because that order is the order
	// the model has to *decide* the fields in and schema authors already put
	// them in a sensible one.
	//
	// Any order is semantically fine — every JSON parser accepts it and the
	// schema does not care — so this costs nothing and buys better generation.
	var req, opt []string
	for _, key := range s.propertyOrder() {
		if required[key] {
			req = append(req, key)
		} else {
			opt = append(opt, key)
		}
	}

	// kv renders one `"key": <rule>` pair.
	kv := func(key string) (string, error) {
		sub, err := c.compile(s.Properties[key], c.fresh(name+"-"+key))
		if err != nil {
			return "", fmt.Errorf("property %q: %w", key, err)
		}
		return fmt.Sprintf(`%s ws ":" ws %s`, quoteGBNF(`"`+key+`"`), sub), nil
	}

	var body string
	switch {
	case len(req) > 0:
		// At least one property is guaranteed to be emitted, so every optional
		// can safely carry its own leading comma.
		var parts []string
		for i, key := range req {
			pair, err := kv(key)
			if err != nil {
				return "", err
			}
			if i > 0 {
				parts = append(parts, `ws "," ws`)
			}
			parts = append(parts, pair)
		}
		for _, key := range opt {
			pair, err := kv(key)
			if err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf(`( ws "," ws %s )?`, pair))
		}
		body = fmt.Sprintf(`"{" ws %s ws "}"`, strings.Join(parts, " "))

	default:
		// Every property is optional, so nothing is guaranteed to precede a
		// comma. Attaching the comma to each optional independently would
		// permit `{, "b": 1}` — invalid JSON that a model decoding under the
		// grammar would happily produce, which is precisely the failure this
		// package exists to make impossible.
		//
		// The fix is an alternation over *which property comes first*: each
		// branch emits one property unconditionally and the rest as
		// comma-prefixed optionals. Quadratic in the number of properties and
		// entirely fine at the sizes schemas actually have.
		alts := make([]string, 0, len(opt))
		for i, key := range opt {
			head, err := kv(key)
			if err != nil {
				return "", err
			}
			parts := []string{head}
			for _, later := range opt[i+1:] {
				pair, err := kv(later)
				if err != nil {
					return "", err
				}
				parts = append(parts, fmt.Sprintf(`( ws "," ws %s )?`, pair))
			}
			alts = append(alts, strings.Join(parts, " "))
		}
		if len(alts) == 0 {
			body = `"{" ws "}"`
		} else {
			body = fmt.Sprintf(`"{" ws ( %s )? ws "}"`, strings.Join(alts, " | "))
		}
	}

	c.emit(name, body)
	return name, nil
}

func (c *compiler) compileArray(s *Schema, name string) (string, error) {
	if s.Items == nil {
		c.emit(name, `"[" ws ( value ( ws "," ws value )* )? ws "]"`)
		return name, nil
	}
	item, err := c.compile(s.Items, c.fresh(name+"-item"))
	if err != nil {
		return "", fmt.Errorf("array items: %w", err)
	}
	c.emit(name, fmt.Sprintf(`"[" ws ( %s ( ws "," ws %s )* )? ws "]"`, item, item))
	return name, nil
}

// primitives are the shared terminal rules every compiled grammar ends with.
//
// Numeric bounds (`minimum`/`maximum`) are deliberately NOT encoded. A grammar
// that expressed "an integer between 8000 and 192000" would be enormous and
// brittle, and the payoff is small: a range violation is caught by the
// validator (see validate.go) on a value the model at least produced in the
// right shape. Constraining shape in the grammar and range in the validator
// puts each check where it is cheap.
const primitives = `
ws     ::= [ \t\n]*
string ::= "\"" char* "\""
char   ::= [^"\\\x7F\x00-\x1F] | "\\" ( ["\\bfnrt/] | "u" hex hex hex hex )
hex    ::= [0-9a-fA-F]
integer ::= "-"? ( "0" | [1-9] [0-9]* )
number ::= integer ( "." [0-9]+ )? ( [eE] [-+]? [0-9]+ )?
boolean ::= "true" | "false"
object ::= "{" ws ( string ws ":" ws value ( ws "," ws string ws ":" ws value )* )? ws "}"
array  ::= "[" ws ( value ( ws "," ws value )* )? ws "]"
value  ::= object | array | string | number | boolean | "null"
`

// quoteGBNF renders a JSON literal as a GBNF string terminal.
func quoteGBNF(jsonLiteral string) string {
	return strconv.Quote(jsonLiteral)
}

var sanitizer = strings.NewReplacer("_", "-", ".", "-", "/", "-", "@", "-")

func sanitize(s string) string {
	out := sanitizer.Replace(strings.ToLower(s))
	if out == "" {
		return "rule"
	}
	return out
}
