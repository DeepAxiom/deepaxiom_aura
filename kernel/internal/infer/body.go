package infer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Editing a request body without re-serialising it.
//
// The plane has to touch a request: to ask the engine for token usage, to set
// a priority, to take out a field that is the node's to decide. The obvious
// way — decode into a map, change it, encode it again — rewrites every byte of
// the body, and Go's encoder does not write what it read: it compacts
// whitespace, escapes `<`, `>` and `&`, and re-encodes every string. For most
// fields nobody would notice. For `messages` it is the whole point: what the
// node says a model was shown has to be what the model was sent, and a hash
// taken over one must match the other.
//
// So the body is kept as the caller's bytes with a map of where each top-level
// member lies. Changing a member replaces that member's value; every member
// that is not changed — `messages`, `tools`, all of it — reaches the engine
// byte for byte as it was written. Only the separators between top-level
// members are the node's own, and only when something was edited: a body
// nothing touched is forwarded as the very slice that was received.
//
// Two bodies are refused outright rather than repaired:
//
//   - **One that is not a JSON object.** There is nothing to be careful with.
//   - **One with a top-level key written twice.** JSON does not say which one
//     counts and parsers disagree. A caller could write `"priority"` twice and
//     have this node read one value while the engine honours the other — a
//     parser differential is a way to say one thing to the checker and another
//     to the thing being checked.

// ErrDuplicateKey reports a top-level key that appears more than once.
var ErrDuplicateKey = errors.New("a top-level key appears more than once")

// member is one top-level entry of the object.
type member struct {
	key string
	// head is the caller's own bytes for the key and its colon, spacing
	// included; val is the caller's own bytes for the value. Either is
	// replaced only by an explicit edit.
	head []byte
	val  []byte
}

// body is a request body and the layout of its top-level object.
type body struct {
	raw     []byte
	members []member
	changed bool
}

// parseBody maps the top level of a JSON object. It reads the whole document,
// so a body that parses here is one an engine's parser will also find
// well-formed.
func parseBody(raw []byte) (*body, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("request body is not JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("request body must be a JSON object")
	}
	b := &body{raw: raw}
	seen := map[string]bool{}
	entry := int(dec.InputOffset()) // where the search for the next key begins

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("request body is not JSON: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("request body is not a JSON object")
		}
		// Compared after decoding, so `"a"` and `"a"` are the same key —
		// which is how every parser downstream will see them.
		if seen[key] {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateKey, key)
		}
		seen[key] = true

		// The key's opening quote is the first `"` after the previous entry:
		// only whitespace and one comma can stand between.
		keyStart := entry + bytes.IndexByte(raw[entry:], '"')

		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, fmt.Errorf("request body is not JSON: %w", err)
		}
		valEnd := int(dec.InputOffset())
		valStart := valEnd - len(val)
		b.members = append(b.members, member{
			key: key, head: raw[keyStart:valStart], val: raw[valStart:valEnd],
		})
		entry = valEnd
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, fmt.Errorf("request body is not JSON: %w", err)
	}
	if rest := bytes.TrimSpace(raw[dec.InputOffset():]); len(rest) != 0 {
		return nil, errors.New("request body has content after its JSON object")
	}
	return b, nil
}

func (b *body) index(key string) int {
	for i := range b.members {
		if b.members[i].key == key {
			return i
		}
	}
	return -1
}

// get returns a top-level value exactly as written.
func (b *body) get(key string) (json.RawMessage, bool) {
	i := b.index(key)
	if i < 0 {
		return nil, false
	}
	return json.RawMessage(b.members[i].val), true
}

// has reports whether a top-level key is present.
func (b *body) has(key string) bool { return b.index(key) >= 0 }

// isTrue reports whether a top-level key is the literal `true`.
func (b *body) isTrue(key string) bool {
	v, ok := b.get(key)
	return ok && string(v) == "true"
}

// set gives a top-level key a value: replacing the value where the key
// exists, appending the member where it does not. value must be valid JSON.
func (b *body) set(key string, value []byte) {
	b.changed = true
	if i := b.index(key); i >= 0 {
		b.members[i].val = value
		return
	}
	head, _ := json.Marshal(key)
	b.members = append(b.members, member{key: key, head: append(head, ':'), val: value})
}

// remove takes a top-level member out.
func (b *body) remove(key string) {
	i := b.index(key)
	if i < 0 {
		return
	}
	b.changed = true
	b.members = append(b.members[:i:i], b.members[i+1:]...)
}

// bytes returns the body to forward. With no edits it is the original slice,
// untouched and uncopied.
func (b *body) bytes() []byte {
	if !b.changed {
		return b.raw
	}
	size := 2
	for _, m := range b.members {
		size += len(m.head) + len(m.val) + 1
	}
	out := make([]byte, 0, size)
	out = append(out, '{')
	for i, m := range b.members {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, m.head...)
		out = append(out, m.val...)
	}
	return append(out, '}')
}

// withUsage returns the `stream_options` value that also asks for usage,
// keeping whatever else the caller put there. Only this one small value is
// ever re-encoded.
func withUsage(existing json.RawMessage) []byte {
	opts := map[string]json.RawMessage{}
	if len(existing) > 0 {
		// A caller-supplied value that is not an object is replaced: the
		// engine would have refused it anyway.
		if json.Unmarshal(existing, &opts) != nil || opts == nil {
			opts = map[string]json.RawMessage{}
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	out, _ := json.Marshal(opts)
	return out
}

// asksForUsage reports whether a `stream_options` value already has
// `include_usage: true` — that is, whether the caller wants the usage chunk
// itself, as opposed to the node having asked on its own account.
func asksForUsage(existing json.RawMessage) bool {
	var opts struct {
		IncludeUsage bool `json:"include_usage"`
	}
	return len(existing) > 0 && json.Unmarshal(existing, &opts) == nil && opts.IncludeUsage
}
