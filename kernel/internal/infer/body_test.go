package infer

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// A body with everything a re-encoder would change: odd spacing, characters
// Go escapes on the way out, an escape that is not the shortest form.
const awkward = `{ "model" : "m",
  "messages":[ {"role":"user","content":"a < b && c > d  é é \/ "} ],
  "tools" : [ {"type":"function" ,"function":{"name":"f"}} ],
  "stream":true }`

func mustParse(t *testing.T, raw string) *body {
	t.Helper()
	b, err := parseBody([]byte(raw))
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	return b
}

func valid(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the edited body is not JSON: %v\n%s", err, raw)
	}
	return out
}

func TestAnUntouchedBodyIsTheSameSlice(t *testing.T) {
	raw := []byte(awkward)
	b, err := parseBody(raw)
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	out := b.bytes()
	if len(out) != len(raw) || &out[0] != &raw[0] {
		t.Fatal("a body nothing edited was copied or changed; it must be forwarded as received")
	}
}

// The property the whole file exists for: whatever is done to the rest of the
// request, `messages` and `tools` reach the engine exactly as written.
func TestEditsNeverTouchTheBytesOfMessagesOrTools(t *testing.T) {
	b := mustParse(t, awkward)
	wantMessages, _ := b.get("messages")
	wantTools, _ := b.get("tools")
	wantMessages = append(json.RawMessage(nil), wantMessages...)
	wantTools = append(json.RawMessage(nil), wantTools...)

	b.set("stream_options", withUsage(nil))
	b.set("priority", []byte("-7"))
	b.remove("model")
	out := b.bytes()

	got := valid(t, out)
	if !bytes.Equal(got["messages"], wantMessages) {
		t.Fatalf("messages changed:\n got %s\nwant %s", got["messages"], wantMessages)
	}
	if !bytes.Equal(got["tools"], wantTools) {
		t.Fatalf("tools changed:\n got %s\nwant %s", got["tools"], wantTools)
	}
	if !bytes.Contains(out, wantMessages) {
		t.Fatal("the bytes of messages are not present verbatim in the forwarded body")
	}
	// What a re-encoder would have done to that same value.
	var decoded any
	_ = json.Unmarshal(wantMessages, &decoded)
	reencoded, _ := json.Marshal(decoded)
	if bytes.Equal(reencoded, wantMessages) {
		t.Fatal("precondition: this test needs a value that re-encoding would change")
	}
}

func TestSetReplacesAppendsAndRemoveRemoves(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		edit     func(*body)
		want     map[string]string
		absent   []string
	}{
		{"replace", `{"a":1,"b":2}`, func(b *body) { b.set("a", []byte("9")) },
			map[string]string{"a": "9", "b": "2"}, nil},
		{"append", `{"a":1}`, func(b *body) { b.set("z", []byte(`"x"`)) },
			map[string]string{"a": "1", "z": `"x"`}, nil},
		{"append to empty", `{}`, func(b *body) { b.set("z", []byte("true")) },
			map[string]string{"z": "true"}, nil},
		{"append twice", `{"a":1}`, func(b *body) { b.set("y", []byte("1")); b.set("z", []byte("2")) },
			map[string]string{"a": "1", "y": "1", "z": "2"}, nil},
		{"remove first", `{"a":1,"b":2,"c":3}`, func(b *body) { b.remove("a") },
			map[string]string{"b": "2", "c": "3"}, []string{"a"}},
		{"remove middle", `{"a":1,"b":2,"c":3}`, func(b *body) { b.remove("b") },
			map[string]string{"a": "1", "c": "3"}, []string{"b"}},
		{"remove last", `{"a":1,"b":2,"c":3}`, func(b *body) { b.remove("c") },
			map[string]string{"a": "1", "b": "2"}, []string{"c"}},
		{"remove only", `{"a":1}`, func(b *body) { b.remove("a") },
			map[string]string{}, []string{"a"}},
		// The case that splicing commas by hand gets wrong: the only member
		// goes and another arrives.
		{"remove only then append", `{"priority":5}`, func(b *body) { b.remove("priority"); b.set("z", []byte("1")) },
			map[string]string{"z": "1"}, []string{"priority"}},
		{"remove absent", `{"a":1}`, func(b *body) { b.remove("nope") },
			map[string]string{"a": "1"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := mustParse(t, tc.in)
			tc.edit(b)
			got := valid(t, b.bytes())
			if len(got) != len(tc.want) {
				t.Fatalf("got %d members, want %d: %s", len(got), len(tc.want), b.bytes())
			}
			for k, v := range tc.want {
				if string(got[k]) != v {
					t.Fatalf("%s = %s, want %s", k, got[k], v)
				}
			}
			for _, k := range tc.absent {
				if _, present := got[k]; present {
					t.Fatalf("%s is still there", k)
				}
			}
		})
	}
}

// A key written twice lets a caller say one thing to this node and another to
// the engine, depending on which copy each parser honours.
func TestAKeyWrittenTwiceIsRefused(t *testing.T) {
	for _, raw := range []string{
		`{"priority":0,"messages":[],"priority":99}`,
		`{"stream":true,"stream":false}`,
		// The same key spelled two ways. Every parser downstream decodes both
		// to "priority"; a check on the raw text would have let it through.
		`{"priority":0,"priority":99}`,
	} {
		if _, err := parseBody([]byte(raw)); !errors.Is(err, ErrDuplicateKey) {
			t.Fatalf("%s: err=%v, want ErrDuplicateKey", raw, err)
		}
	}
}

func TestWhatIsNotAJSONObjectIsRefused(t *testing.T) {
	for _, raw := range []string{
		``, `[]`, `"text"`, `42`, `null`, `{`, `{"a":1`, `{"a":}`,
		`{"a":1} trailing`, `{"a":1}{"b":2}`, `{"a":1},`,
	} {
		if _, err := parseBody([]byte(raw)); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
	// Trailing whitespace is not content.
	if _, err := parseBody([]byte("{\"a\":1}\n  \n")); err != nil {
		t.Fatalf("trailing whitespace refused: %v", err)
	}
}

func TestNestedDuplicatesAreNotThisNodesBusiness(t *testing.T) {
	// Only the top level is the node's to read. What is inside `messages` is
	// forwarded as written and judged by the engine.
	if _, err := parseBody([]byte(`{"messages":[{"role":"user","role":"system"}]}`)); err != nil {
		t.Fatalf("a nested duplicate was refused: %v", err)
	}
}

func TestAskingForUsageKeepsWhatTheCallerAsked(t *testing.T) {
	got := valid(t, withUsage(json.RawMessage(`{"continuous_usage_stats":true}`)))
	if string(got["include_usage"]) != "true" || string(got["continuous_usage_stats"]) != "true" {
		t.Fatalf("stream_options became %v", got)
	}
	if got := valid(t, withUsage(nil)); string(got["include_usage"]) != "true" {
		t.Fatalf("stream_options became %v", got)
	}
	// Not an object: replaced rather than merged into.
	if got := valid(t, withUsage(json.RawMessage(`"yes"`))); string(got["include_usage"]) != "true" {
		t.Fatalf("stream_options became %v", got)
	}
}

func TestWhetherTheCallerAskedForUsageItself(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"include_usage":true}`:  true,
		`{"include_usage":false}`: false,
		`{}`:                      false,
		``:                        false,
		`"nonsense"`:              false,
	} {
		if got := asksForUsage(json.RawMessage(raw)); got != want {
			t.Fatalf("asksForUsage(%s) = %v, want %v", raw, got, want)
		}
	}
}
