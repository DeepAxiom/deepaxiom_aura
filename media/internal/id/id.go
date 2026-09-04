// Package id mints the identifiers this subsystem hands out.
//
// Same shape as the ids the kernel and the Python SDK use — Crockford base32,
// time prefix first — so an asset id sorts by creation and reads the same in a
// ledger entry, a job row and a log line. This module does not import the
// kernel, so the generator lives here rather than being shared: twenty lines
// duplicated is cheaper than a module dependency between two artefacts that
// are versioned apart on purpose.
package id

import (
	"crypto/rand"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a lexicographically sortable unique id.
func New() string {
	ms := time.Now().UnixMilli()
	out := make([]byte, 26)
	for i := 9; i >= 0; i-- {
		out[i] = alphabet[ms&31]
		ms >>= 5
	}
	var buf [16]byte
	// crypto/rand.Read never returns an error; it panics on a broken source.
	_, _ = rand.Read(buf[:])
	for i, b := range buf {
		out[10+i] = alphabet[b&31]
	}
	return string(out)
}
