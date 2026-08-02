// Package channel implements contract C3 — the channel protocol.
package channel

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"aura/kernel/internal/spec"
)

// ProtocolMajor is the C3 protocol major this kernel speaks.
const ProtocolMajor = "1"

// QoS classes a graph declares per edge (C3 rule 4).
//
// The difference is what happens under pressure. A `reliable` edge blocks its
// producer until the receiver catches up: nothing is lost, but everything
// waits. A `realtime` edge drops the oldest frame instead, because in a live
// stream a late frame is worthless — a voice channel that buffers is a voice
// channel you cannot interrupt.
//
// `bulk` is named by the spec but its behaviour is not defined yet; it is
// treated as `reliable` until it is.
const (
	QoSReliable = spec.QoSReliable
	QoSRealtime = spec.QoSRealtime
	QoSBulk     = spec.QoSBulk
)

// Envelope kinds (C3).
const (
	KindData            = spec.KindData
	KindDone            = spec.KindDone
	KindError           = spec.KindError
	KindStatus          = spec.KindStatus
	KindRegister        = spec.KindRegister
	KindConfirmRequest  = spec.KindConfirmRequest
	KindConfirmResponse = spec.KindConfirmResponse
	KindCancel          = spec.KindCancel       // best-effort abort request — see spec/c3-channel.md
	KindConfigUpdate    = spec.KindConfigUpdate // kernel -> skill: runtime config changed live
)

// Envelope is the unit of everything that flows through the system (C3).
type Envelope struct {
	V       string          `json:"v"`
	ID      string          `json:"id"`
	CauseID string          `json:"cause_id,omitempty"`
	Session string          `json:"session,omitempty"`
	Node    string          `json:"node,omitempty"`
	Port    string          `json:"port,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Idem    string          `json:"idem,omitempty"`
	Schema  string          `json:"schema,omitempty"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Receipt is set only on an envelope the kernel delivered into a motor
	// skill: the C4 ledger entry hash that sealed that effect. Absent on
	// every envelope that carries no effect, which is most of them.
	Receipt string `json:"receipt,omitempty"`
}

// Validate enforces the normative envelope rules.
func (e *Envelope) Validate() error {
	if e.V != ProtocolMajor {
		return fmt.Errorf("unsupported protocol major %q (kernel speaks %s)", e.V, ProtocolMajor)
	}
	if e.ID == "" {
		return fmt.Errorf("envelope missing id")
	}
	if e.Kind == "" {
		return fmt.Errorf("envelope missing kind")
	}
	if e.Kind == KindData {
		switch {
		case e.Session == "":
			return fmt.Errorf("data envelope missing session")
		case e.Node == "" || e.Port == "":
			return fmt.Errorf("data envelope missing node/port")
		case e.Idem == "":
			return fmt.Errorf("data envelope missing idempotency key")
		case e.Schema == "":
			return fmt.Errorf("data envelope missing schema")
		}
	}
	return nil
}

// NewID returns a lexicographically sortable unique id (ULID-like:
// millisecond timestamp + random suffix, Crockford base32).
func NewID() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	now := uint64(time.Now().UnixMilli())
	buf := make([]byte, 16)
	ts := make([]byte, 10)
	for i := 9; i >= 0; i-- {
		ts[i] = alphabet[now&31]
		now >>= 5
	}
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	rnd := make([]byte, 16)
	for i, b := range buf {
		rnd[i] = alphabet[int(b)&31]
	}
	return string(ts) + string(rnd)
}

// Dedup is a bounded at-least-once deduplicator keyed by idem (C3 rule 2).
type Dedup struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
	max   int
}

func NewDedup(max int) *Dedup {
	return &Dedup{seen: make(map[string]struct{}, max), max: max}
}

// Seen records key and reports whether it was already present.
func (d *Dedup) Seen(key string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[key]; ok {
		return true
	}
	d.seen[key] = struct{}{}
	d.order = append(d.order, key)
	if len(d.order) > d.max {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}
	return false
}
