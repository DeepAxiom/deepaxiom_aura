package grammar

import (
	"encoding/json"
	"testing"
)

// grammarFor compiles a schema literal and returns the GBNF.
func grammarFor(t *testing.T, schema string) string {
	t.Helper()
	s, err := Parse([]byte(schema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	g, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return g
}

// assertGrammar checks that the grammar accepts everything in `good` and
// rejects everything in `bad`.
//
// Every `good` case is also checked to be valid JSON, so a test cannot pass by
// asserting the grammar accepts something that was never legal anyway.
func assertGrammar(t *testing.T, gbnf string, good, bad []string) {
	t.Helper()
	for _, in := range good {
		if !json.Valid([]byte(in)) {
			t.Fatalf("test bug: %q is not valid JSON but is listed as good", in)
		}
		if !accepts(t, gbnf, in) {
			t.Errorf("grammar REJECTED valid input %q", in)
		}
	}
	for _, in := range bad {
		if accepts(t, gbnf, in) {
			t.Errorf("grammar ACCEPTED invalid input %q", in)
		}
	}
}

// The headline property: a model decoding under a port's grammar cannot emit
// a document the port would reject.
func TestGrammarForStdText(t *testing.T) {
	g := grammarFor(t, stdSchema("std/text@1"))
	assertGrammar(t, g,
		[]string{
			`{"text":"hello"}`,
			`{"text":"hello","final":true}`,
			`{"text":""}`,
			`{ "text" : "spaced" , "final" : false }`,
			`{"text":"esc \"quoted\" and \\ back"}`,
		},
		[]string{
			`{}`,                         // text is required
			`{"final":true}`,             // required property missing
			`{"text":42}`,                // wrong type
			`{"text":"a",}`,              // trailing comma
			`{"text":"a" "final":true}`,  // missing comma
			`{"final":true,"text":"a"}`,  // required must precede optional
			`{"text":"a","final":"yes"}`, // boolean expected
			`{"text":"a","unknown":1}`,   // undeclared key is not in the grammar
			`"text"`,                     // not an object
		})
}

// std/status@1 has an enum, the one case where a grammar is strictly stronger
// than validating after the fact: the wrong value is unreachable.
func TestGrammarEnforcesEnums(t *testing.T) {
	g := grammarFor(t, stdSchema("std/status@1"))
	assertGrammar(t, g,
		[]string{
			`{"state":"working"}`,
			`{"state":"done","detail":"finished"}`,
			`{"state":"error","detail":""}`,
		},
		[]string{
			`{"state":"nope"}`,
			`{"state":"Working"}`, // case matters
			`{"state":true}`,
			`{"detail":"x"}`,
		})
}

// The regression this package's first build shipped with: a schema whose
// properties are ALL optional. Attaching a comma to each optional
// independently permits `{, "b": 1}` — invalid JSON that a model decoding
// under the grammar would happily produce.
func TestGrammarWithOnlyOptionalProperties(t *testing.T) {
	g := grammarFor(t, `{
	  "type": "object",
	  "properties": {
	    "a": { "type": "string" },
	    "b": { "type": "integer" },
	    "c": { "type": "boolean" }
	  }
	}`)
	assertGrammar(t, g,
		[]string{
			`{}`,
			`{"a":"x"}`,
			`{"b":1}`,
			`{"c":true}`,
			`{"a":"x","b":1}`,
			`{"a":"x","c":true}`,
			`{"b":1,"c":false}`,
			`{"a":"x","b":1,"c":true}`,
		},
		[]string{
			`{,"b":1}`,           // the bug
			`{"b":1,}`,           // trailing comma
			`{"c":true,"a":"x"}`, // out of the grammar's declared order
			`{"a":1}`,            // wrong type
			`{"d":1}`,            // undeclared
		})
}

// std/api-request@1 is the real schema with no required properties, so this
// pins the bug against a document that actually ships.
func TestGrammarForStdAPIRequest(t *testing.T) {
	g := grammarFor(t, stdSchema("std/api-request@1"))
	assertGrammar(t, g,
		[]string{
			`{}`,
			`{"body":{"amount":100}}`,
			`{"params":{"id":"7"}}`,
			`{"params":{},"query":{},"headers":{},"body":null}`,
		},
		[]string{
			`{,"body":1}`,
			`{"params":"not-an-object"}`,
		})
}

// Declaration order, not alphabetical: std/transcript@1 declares
// text, final, replace, utterance, confidence — so a model emits the text
// before the flag describing it, rather than having to commit to `final`
// before writing what it is final about.
func TestGrammarForStdTranscript(t *testing.T) {
	g := grammarFor(t, stdSchema("std/transcript@1"))
	assertGrammar(t, g,
		[]string{
			`{"text":"hola","final":true}`,
			`{"text":"ho","final":false,"confidence":0.8}`,
			`{"text":"hola","final":true,"replace":true,"utterance":"u1","confidence":1}`,
		},
		[]string{
			`{"text":"hola"}`,              // final is required
			`{"final":true}`,               // text is required
			`{"final":true,"text":"hola"}`, // declared order is text, final
			`{"text":"a","final":true,"confidence":"high"}`,
		})
}

// The order the grammar imposes is the schema's own, which is what makes the
// generated JSON read the way the schema's examples do.
func TestGrammarUsesDeclarationOrderNotAlphabetical(t *testing.T) {
	g := grammarFor(t, `{
	  "type": "object",
	  "required": ["zebra", "apple"],
	  "properties": {
	    "zebra": { "type": "string" },
	    "apple": { "type": "string" },
	    "yak":   { "type": "string" },
	    "bee":   { "type": "string" }
	  }
	}`)
	assertGrammar(t, g,
		[]string{
			`{"zebra":"z","apple":"a"}`,
			`{"zebra":"z","apple":"a","yak":"y"}`,
			`{"zebra":"z","apple":"a","yak":"y","bee":"b"}`,
			`{"zebra":"z","apple":"a","bee":"b"}`,
		},
		[]string{
			`{"apple":"a","zebra":"z"}`,                     // alphabetical, not declared
			`{"zebra":"z","apple":"a","bee":"b","yak":"y"}`, // optionals out of order
		})
}

func TestGrammarForArrays(t *testing.T) {
	g := grammarFor(t, `{
	  "type": "object",
	  "required": ["tags"],
	  "properties": {
	    "tags": { "type": "array", "items": { "type": "string" } }
	  }
	}`)
	assertGrammar(t, g,
		[]string{
			`{"tags":[]}`,
			`{"tags":["a"]}`,
			`{"tags":["a","b","c"]}`,
			`{"tags":[ "a" , "b" ]}`,
		},
		[]string{
			`{"tags":[1]}`,
			`{"tags":["a",]}`,
			`{"tags":"a"}`,
		})
}

// Every schema the runtime actually ships must compile, and the grammar must
// accept a document the validator also accepts. This is the pair that has to
// agree: grammar (what can be produced) and validator (what is accepted).
func TestEveryStdSchemaCompiles(t *testing.T) {
	r := NewRegistry()
	refs := r.Refs()
	if len(refs) < 8 {
		t.Fatalf("expected the std namespace to be embedded, got %d refs", len(refs))
	}
	for _, ref := range refs {
		if _, err := r.Grammar(ref); err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
}

// An empty schema means "any JSON", and a grammar that constrained it would
// reject documents the port accepts.
func TestEmptySchemaAcceptsAnyJSON(t *testing.T) {
	g := grammarFor(t, `{}`)
	assertGrammar(t, g,
		[]string{`{}`, `[]`, `"s"`, `42`, `true`, `null`, `{"a":[1,{"b":null}]}`},
		[]string{`{`, `undefined`})
}

func TestUnsupportedTypeIsRefusedNotIgnored(t *testing.T) {
	s, err := Parse([]byte(`{"type":"tuple"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(s); err == nil {
		t.Fatal("an unsupported type compiled; a permissive grammar is worse than none")
	}
}

func TestRequiredPropertyMustBeDeclared(t *testing.T) {
	s, err := Parse([]byte(`{"type":"object","required":["ghost"],"properties":{"a":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(s); err == nil {
		t.Fatal("a schema requiring an undeclared property compiled")
	}
}

// stdSchema returns an embedded std schema document. Named to avoid
// colliding with the imported `spec` package.
func stdSchema(ref string) string {
	r := NewRegistry()
	doc, ok := r.Source(ref)
	if !ok {
		panic("unknown schema ref in test: " + ref)
	}
	return doc
}
