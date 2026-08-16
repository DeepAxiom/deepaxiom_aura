package grammar

// Payload validation against a port's declared schema.
//
// The grammar (gbnf.go) makes an invalid payload *unreachable* for a model
// that decodes under it. This is the other half: it makes an invalid payload
// *rejected* for everything else — a skill that hand-builds a dict, a
// projected API that returned a surprise, a wasm guest, a hostile client.
//
// The two are complementary rather than redundant. A guarantee that only holds
// when the producer cooperates is not a type system; C3 calls channels
// "typed", and until now nothing enforced that claim at the boundary. This is
// what makes the word honest.
//
// # Deliberately a subset
//
// This is not a general JSON Schema validator, and adding a dependency to get
// one would be the wrong trade for a kernel that ships as a single static
// binary with four dependencies. It covers exactly what the `std` namespace
// uses — type, required, properties, enum, minimum/maximum, array items — and
// **reports anything it cannot check rather than passing it**. A validator
// that silently ignores a keyword is worse than no validator, because callers
// build on a guarantee that is not there.

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Problem is one way a payload failed its port's schema.
type Problem struct {
	// Path is a JSON-pointer-ish location: "" for the root, "text" for a
	// property, "inputs[2].port" inside an array.
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Detail
	}
	return p.Path + ": " + p.Detail
}

// Validate checks a payload against a schema and returns every problem found.
//
// Every problem, not the first: a skill author fixing output shape wants the
// whole list, and the cost of continuing is nil on documents this size.
func Validate(s *Schema, payload []byte) []Problem {
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return []Problem{{Detail: "payload is not valid JSON: " + err.Error()}}
	}
	var out []Problem
	validate(s, v, "", &out)
	return out
}

// Explain renders problems as one line, for an error message.
func Explain(problems []Problem) string {
	parts := make([]string, len(problems))
	for i, p := range problems {
		parts[i] = p.String()
	}
	return strings.Join(parts, "; ")
}

func validate(s *Schema, v any, path string, out *[]Problem) {
	if s == nil {
		return
	}

	if len(s.Enum) > 0 {
		for _, allowed := range s.Enum {
			if jsonEqual(allowed, v) {
				return
			}
		}
		*out = append(*out, Problem{path, fmt.Sprintf(
			"value %s is not one of %s", render(v), render(s.Enum))})
		return
	}

	switch s.Type {
	case "":
		// No constraint declared: anything is valid, and saying so explicitly
		// keeps this from looking like an accidental gap.
		return

	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			*out = append(*out, Problem{path, fmt.Sprintf("expected an object, got %s", kindOf(v))})
			return
		}
		for _, key := range sortedRequired(s) {
			if _, present := m[key]; !present {
				*out = append(*out, Problem{path, fmt.Sprintf("missing required property %q", key)})
			}
		}
		for _, key := range sortedKeys(m) {
			sub, declared := s.Properties[key]
			if !declared {
				// Undeclared properties are permitted unless the schema said
				// otherwise, which is JSON Schema's default and what the std
				// namespace relies on for forward compatibility.
				if s.AdditionalProperties != nil && !*s.AdditionalProperties {
					*out = append(*out, Problem{path, fmt.Sprintf(
						"property %q is not declared and additionalProperties is false", key)})
				}
				continue
			}
			validate(sub, m[key], join(path, key), out)
		}

	case "array":
		items, ok := v.([]any)
		if !ok {
			*out = append(*out, Problem{path, fmt.Sprintf("expected an array, got %s", kindOf(v))})
			return
		}
		if s.Items != nil {
			for i, item := range items {
				validate(s.Items, item, fmt.Sprintf("%s[%d]", path, i), out)
			}
		}

	case "string":
		if _, ok := v.(string); !ok {
			*out = append(*out, Problem{path, fmt.Sprintf("expected a string, got %s", kindOf(v))})
		}

	case "boolean":
		if _, ok := v.(bool); !ok {
			*out = append(*out, Problem{path, fmt.Sprintf("expected a boolean, got %s", kindOf(v))})
		}

	case "integer":
		n, ok := v.(float64)
		if !ok {
			*out = append(*out, Problem{path, fmt.Sprintf("expected an integer, got %s", kindOf(v))})
			return
		}
		if n != math.Trunc(n) {
			*out = append(*out, Problem{path, fmt.Sprintf("expected an integer, got %v", n)})
			return
		}
		checkBounds(s, n, path, out)

	case "number":
		n, ok := v.(float64)
		if !ok {
			*out = append(*out, Problem{path, fmt.Sprintf("expected a number, got %s", kindOf(v))})
			return
		}
		checkBounds(s, n, path, out)

	case "null":
		if v != nil {
			*out = append(*out, Problem{path, fmt.Sprintf("expected null, got %s", kindOf(v))})
		}

	default:
		// The honest branch. An unrecognised type is reported, never passed:
		// silently accepting means claiming a check that did not happen.
		*out = append(*out, Problem{path, fmt.Sprintf(
			"schema declares type %q, which this validator does not implement — "+
				"treating it as a failure rather than claiming a check it did not make",
			s.Type)})
	}
}

func checkBounds(s *Schema, n float64, path string, out *[]Problem) {
	if s.Minimum != nil && n < *s.Minimum {
		*out = append(*out, Problem{path, fmt.Sprintf("%v is below the minimum %v", n, *s.Minimum)})
	}
	if s.Maximum != nil && n > *s.Maximum {
		*out = append(*out, Problem{path, fmt.Sprintf("%v is above the maximum %v", n, *s.Maximum)})
	}
}

func sortedRequired(s *Schema) []string {
	out := append([]string(nil), s.Required...)
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return "an unknown value"
}

func render(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	if len(raw) > 60 {
		return string(raw[:57]) + "…"
	}
	return string(raw)
}

// jsonEqual compares two decoded JSON values structurally. Enum members come
// from the schema document and the value from the payload, so both are already
// in Go's generic JSON representation and a shallow comparison suffices for
// the scalar members the std namespace uses.
func jsonEqual(a, b any) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ra) == string(rb)
}
