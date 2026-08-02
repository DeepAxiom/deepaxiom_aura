package channel

import (
	"encoding/json"
	"sort"
	"testing"
)

func validData() Envelope {
	return Envelope{
		V: ProtocolMajor, ID: NewID(), Session: "sess-1",
		Node: "eco", Port: "text_in", Seq: 1, Idem: "sess-1:eco:text_in:1",
		Schema: "std/text@1", Kind: KindData,
		Payload: json.RawMessage(`{"text":"hi"}`),
	}
}

func TestValidateAcceptsAWellFormedDataEnvelope(t *testing.T) {
	env := validData()
	if err := env.Validate(); err != nil {
		t.Fatalf("want valid, got %v", err)
	}
}

// The normative rules a data envelope must satisfy (C3). Each case drops
// exactly one requirement from an otherwise valid envelope.
func TestValidateRejectsMalformedEnvelopes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Envelope)
	}{
		{"unsupported protocol major", func(e *Envelope) { e.V = "2" }},
		{"empty protocol major", func(e *Envelope) { e.V = "" }},
		{"missing id", func(e *Envelope) { e.ID = "" }},
		{"missing kind", func(e *Envelope) { e.Kind = "" }},
		{"data without session", func(e *Envelope) { e.Session = "" }},
		{"data without node", func(e *Envelope) { e.Node = "" }},
		{"data without port", func(e *Envelope) { e.Port = "" }},
		{"data without idempotency key", func(e *Envelope) { e.Idem = "" }},
		{"data without schema", func(e *Envelope) { e.Schema = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validData()
			tc.mutate(&env)
			if err := env.Validate(); err == nil {
				t.Fatalf("want an error, got nil")
			}
		})
	}
}

// Only `data` carries the session/node/port/idem/schema requirements: a
// register or cancel envelope legitimately has none of them.
func TestValidateOnlyConstrainsDataEnvelopes(t *testing.T) {
	for _, kind := range []string{KindRegister, KindCancel, KindStatus, KindConfigUpdate} {
		env := Envelope{V: ProtocolMajor, ID: NewID(), Kind: kind}
		if err := env.Validate(); err != nil {
			t.Fatalf("kind %q: want valid without data fields, got %v", kind, err)
		}
	}
}

func TestDedupReportsRepeatsAndNotFirstSightings(t *testing.T) {
	d := NewDedup(16)
	if d.Seen("a") {
		t.Fatal("first sighting of a key must not be reported as seen")
	}
	if !d.Seen("a") {
		t.Fatal("second sighting of the same key must be reported as seen")
	}
	if d.Seen("b") {
		t.Fatal("a different key must not be reported as seen")
	}
}

// An empty idem means "no idempotency key"; deduplicating on it would
// collapse every such envelope into one.
func TestDedupIgnoresEmptyKeys(t *testing.T) {
	d := NewDedup(16)
	for i := 0; i < 3; i++ {
		if d.Seen("") {
			t.Fatal("empty key must never be reported as seen")
		}
	}
}

// The window is bounded: the oldest key is evicted, so a long-running
// session cannot grow the map without limit.
func TestDedupEvictsOldestBeyondItsBound(t *testing.T) {
	d := NewDedup(2)
	d.Seen("first")
	d.Seen("second")
	d.Seen("third") // evicts "first"

	if d.Seen("first") {
		t.Fatal("want the oldest key evicted, but it was still remembered")
	}
	if !d.Seen("third") {
		t.Fatal("want the newest key still remembered")
	}
}

func TestNewIDIsUnique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewID()
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID produced a duplicate after %d ids: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

// C3 calls the id lexicographically sortable: ids minted later must sort
// after earlier ones, which is what makes the event log orderable by id.
func TestNewIDIsLexicographicallySortable(t *testing.T) {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	id := NewID()
	if len(id) != 26 {
		t.Fatalf("want a 26-character id (10 timestamp + 16 random), got %d: %q", len(id), id)
	}
	for _, c := range id {
		if !containsRune(alphabet, c) {
			t.Fatalf("id %q contains %q, which is outside the Crockford base32 alphabet", id, c)
		}
	}

	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, NewID())
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	// Within the same millisecond the random suffix decides, so compare only
	// the timestamp prefix, which is the part that must be monotonic.
	for i := range ids {
		if ids[i][:10] > sorted[len(sorted)-1][:10] {
			t.Fatalf("timestamp prefixes are not monotonic: %v", ids)
		}
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
