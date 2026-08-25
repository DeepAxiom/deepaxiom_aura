package ledger

import (
	"fmt"

	"aura/kernel/internal/signing"
	"aura/kernel/internal/store"
)

// Key succession (C4 v1.6) — how a node's signing key is replaced without
// orphaning everything it ever signed.
//
// # The problem
//
// A checkpoint is a signature over a head. Before this existed, a node had one
// key forever, so `aura verify` could take one public key and check every
// checkpoint against it. That made rotation impossible in the only sense that
// matters: an operator who suspected their key was compromised could generate a
// new one, and every checkpoint the node had ever signed would then fail to
// verify. The choice was "keep using a key you no longer trust" or "throw away
// your evidence", and an operator facing that choice keeps using the key.
//
// # The record
//
// A succession is one key handing over to the next at a named sequence, signed
// by **both**: the outgoing key authorizes the handover, the incoming key
// proves someone holds it. See store.SuccessionRow for why neither signature
// is redundant.
//
// # Verifying without being told the old keys
//
// The verifier is given exactly one public key — the node's current one, which
// is what sits in identity/ed25519.pub and what an auditor would be handed. It
// does not need a roster of retired keys, and asking it to trust one would
// defeat the point.
//
// Instead it walks the chain **backwards**: find the succession whose
// `to_pubkey` is the key I trust, check both signatures, and now I trust its
// `from_pubkey` for everything at or before its sequence. Repeat until no
// succession points at the key in hand. Each step is a cryptographic
// implication from a key already trusted to one that was, so the whole history
// unfolds from a single trusted starting point.
//
// This is also why the walk starts from the *current* key rather than the
// oldest. Starting at the root would let anyone who appends a succession from
// a key they hold extend the chain forwards into their own key. Starting at
// the key the verifier independently holds means an attacker would have to
// forge a signature by a key they do not have.
//
// # What rotation does not do, stated plainly
//
// It bounds future damage. It cannot un-sign what a compromised key already
// signed: every checkpoint that key produced still verifies, because it really
// did sign them. If the key leaked at time T, everything after T is suspect
// and no rotation changes that — only a witness countersignature obtained
// before T narrows the window. The command says so out loud, and so does C4.

// SuccessionPayload is the domain-separated byte string both signatures cover.
//
// The prefix keeps a succession signature from ever being replayed as a
// checkpoint signature (`aura-ledger-checkpoint-v*`), a package-publish
// signature (`aura-pkg-v1`), or an approval — all of which may use the same
// Ed25519 key. Binding `seq` into the payload means a succession cannot be
// lifted and re-presented at a different point in the history.
func SuccessionPayload(seq uint64, fromPubkey, toPubkey string, ts int64) []byte {
	return []byte(fmt.Sprintf("aura-ledger-succession-v1:%d:%s:%s:%d",
		seq, fromPubkey, toPubkey, ts))
}

// VerifySuccession checks both signatures on one handover.
func VerifySuccession(rec store.SuccessionRow) error {
	if rec.FromPubkey == rec.ToPubkey {
		return fmt.Errorf("succession at seq %d hands a key over to itself", rec.Seq)
	}
	payload := SuccessionPayload(rec.Seq, rec.FromPubkey, rec.ToPubkey, rec.TS)
	if err := signing.Verify(rec.FromPubkey, rec.FromSig, payload); err != nil {
		return fmt.Errorf("succession at seq %d is not signed by the outgoing key %s: %w",
			rec.Seq, Fingerprint(rec.FromPubkey), err)
	}
	if err := signing.Verify(rec.ToPubkey, rec.ToSig, payload); err != nil {
		return fmt.Errorf("succession at seq %d is not signed by the incoming key %s: %w",
			rec.Seq, Fingerprint(rec.ToPubkey), err)
	}
	return nil
}

// KeySegment is one key's tenure: it signed everything from the previous
// segment's end up to and including UpTo. The final segment runs to the end of
// the ledger and carries UpTo = maxSeq.
type KeySegment struct {
	UpTo   uint64 `json:"up_to"`
	Pubkey string `json:"pubkey"`
}

// KeyTimeline answers "which key was in force at sequence N".
type KeyTimeline struct {
	// Segments in ascending sequence order. Always at least one — a node that
	// never rotated has exactly one, covering everything.
	Segments []KeySegment `json:"segments"`
	// Rotations is len(Segments)-1, surfaced so a report can say "this node
	// rotated twice" without the caller doing arithmetic.
	Rotations int `json:"rotations"`
}

const maxSeq = ^uint64(0)

// KeyAt returns the public key in force when the entry at seq was sealed.
func (t KeyTimeline) KeyAt(seq uint64) string {
	for _, seg := range t.Segments {
		if seq <= seg.UpTo {
			return seg.Pubkey
		}
	}
	// Unreachable while the last segment ends at maxSeq, which BuildKeyTimeline
	// guarantees; returning the newest key is the safe reading either way.
	if n := len(t.Segments); n > 0 {
		return t.Segments[n-1].Pubkey
	}
	return ""
}

// BuildKeyTimeline reconstructs the key history backwards from the one key the
// verifier trusts.
//
// It refuses rather than guesses. A succession that does not verify, a fork, a
// cycle, or a record that no chain from the trusted key reaches are all
// evidence that somebody wrote to this table who should not have, and a
// timeline built by ignoring them would launder exactly the tampering the
// ledger exists to catch.
func BuildKeyTimeline(successions []store.SuccessionRow, trustedPubkey string) (KeyTimeline, error) {
	if trustedPubkey == "" {
		return KeyTimeline{}, fmt.Errorf("no public key to anchor the timeline to")
	}
	if len(successions) == 0 {
		return KeyTimeline{Segments: []KeySegment{{UpTo: maxSeq, Pubkey: trustedPubkey}}}, nil
	}

	byTo := make(map[string]store.SuccessionRow, len(successions))
	for _, rec := range successions {
		if prev, clash := byTo[rec.ToPubkey]; clash {
			return KeyTimeline{}, fmt.Errorf(
				"two successions hand over to the same key %s (at seq %d and %d) — "+
					"the chain of custody forks, which no honest node produces",
				Fingerprint(rec.ToPubkey), prev.Seq, rec.Seq)
		}
		byTo[rec.ToPubkey] = rec
	}

	// Walk back from the trusted key, collecting segments newest-first. The
	// loop is bounded by the number of records, so a cycle terminates as an
	// error rather than as a hang.
	reversed := []KeySegment{{UpTo: maxSeq, Pubkey: trustedPubkey}}
	seen := map[string]bool{trustedPubkey: true}
	current := trustedPubkey
	used := 0

	for used <= len(successions) {
		rec, ok := byTo[current]
		if !ok {
			break // reached the node's original key
		}
		if err := VerifySuccession(rec); err != nil {
			return KeyTimeline{}, err
		}
		if seen[rec.FromPubkey] {
			return KeyTimeline{}, fmt.Errorf(
				"the succession records form a cycle at key %s — they cannot describe a history",
				Fingerprint(rec.FromPubkey))
		}
		seen[rec.FromPubkey] = true
		reversed = append(reversed, KeySegment{UpTo: rec.Seq, Pubkey: rec.FromPubkey})
		current = rec.FromPubkey
		used++
	}

	if used != len(successions) {
		return KeyTimeline{}, fmt.Errorf(
			"%d of %d succession records are not reachable from this node's current key — "+
				"either the key given does not belong to this ledger, or rows were added by "+
				"something other than `aura rotate`",
			len(successions)-used, len(successions))
	}

	// Flip to ascending, and check the sequences descend as we walked back.
	// An older key whose tenure ends *after* a newer one's would be a record
	// written out of order.
	segments := make([]KeySegment, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		seg := reversed[i]
		if n := len(segments); n > 0 && seg.UpTo <= segments[n-1].UpTo {
			return KeyTimeline{}, fmt.Errorf(
				"succession sequences do not increase (%d after %d) — the records are out of order",
				seg.UpTo, segments[n-1].UpTo)
		}
		segments = append(segments, seg)
	}
	return KeyTimeline{Segments: segments, Rotations: len(segments) - 1}, nil
}
