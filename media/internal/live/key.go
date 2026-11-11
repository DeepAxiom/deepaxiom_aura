package live

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// A stream key is the only thing standing between a stranger and somebody
// else's channel, and the protocol it travels in gives it no help: RTMP has no
// header for a credential, so the key is the stream name, in the URL. What can
// be done about that is done here.
//
// The key is `<stream id>_<secret>`. The id half is public and is how the row
// is found; the secret half is 192 random bits. Only the SHA-256 of the whole
// key is stored, so a copy of the database is not a list of keys, and a plain
// fast hash is the right one: the thing being hashed is not a password someone
// chose, and there is no dictionary to run against it.

// keySeparator divides the id from the secret. Ids are Crockford base32 and
// the secret is hex, so neither half can contain it.
const keySeparator = "_"

// NewKey mints a key for a stream and returns it with the hash to store. The
// key is returned to the caller once and exists nowhere else afterwards.
func NewKey(streamID string) (key, hash string, err error) {
	var secret [24]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	key = streamID + keySeparator + hex.EncodeToString(secret[:])
	return key, HashKey(key), nil
}

// HashKey is what is stored in place of a key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// KeyStreamID is the public half of a key: which stream it claims to be for.
// It says nothing about whether the key is right.
func KeyStreamID(key string) (string, bool) {
	id, secret, ok := strings.Cut(key, keySeparator)
	if !ok || id == "" || secret == "" || len(key) > 256 {
		return "", false
	}
	return id, true
}

// KeyMatches reports whether a presented key is the one whose hash was stored.
//
// The comparison is of two digests and is constant time. Comparing the
// presented key's digest rather than the key means the time taken does not
// depend on how much of the secret was right, and doing that comparison in
// constant time means it does not depend on how much of the digest was either.
func KeyMatches(presented, storedHash string) bool {
	got := HashKey(presented)
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}
