package live

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"aura/media/internal/rtmp"
)

func TestAKeyIsMintedOnceAndOnlyItsHashIsKept(t *testing.T) {
	key, hash, err := NewKey("01STREAM")
	if err != nil {
		t.Fatal(err)
	}
	// The public half is how the row is found.
	if id, ok := KeyStreamID(key); !ok || id != "01STREAM" {
		t.Errorf("the key %q does not name its stream: %q, %v", key, id, ok)
	}
	// 192 bits of secret, in hex.
	secret := strings.TrimPrefix(key, "01STREAM_")
	if len(secret) != 48 {
		t.Errorf("the secret half is %d characters, want 48", len(secret))
	}
	// What is stored is not the key and does not contain it.
	if hash == key || strings.Contains(hash, secret) || len(hash) != 64 {
		t.Errorf("the stored form is %q", hash)
	}
	if hash != HashKey(key) {
		t.Error("NewKey's hash is not HashKey of its key")
	}
	if !KeyMatches(key, hash) {
		t.Error("the key does not match its own hash")
	}
	// Two keys for the same stream are different keys.
	other, otherHash, _ := NewKey("01STREAM")
	if other == key || otherHash == hash {
		t.Error("two keys minted for one stream are the same")
	}
	if KeyMatches(other, hash) {
		t.Error("another key for the same stream was accepted")
	}
}

func TestOnlyTheExactKeyMatches(t *testing.T) {
	key, hash, _ := NewKey("01STREAM")
	last := key[len(key)-1]
	flipped := byte('0')
	if last == '0' {
		flipped = '1'
	}
	wrong := []string{
		"",
		"01STREAM",
		"01STREAM_",
		key[:len(key)-1],                   // one character short
		key + "0",                          // one long
		key[:len(key)-1] + string(flipped), // last character wrong
		strings.ToUpper(key),
		" " + key,
		key + "\n",
		hash, // the stored form is not a credential: a stolen database is not a list of keys
		"01STREAM_" + hash,
	}
	for _, presented := range wrong {
		if presented == key {
			continue // ToUpper of an all-digit secret, in principle
		}
		if KeyMatches(presented, hash) {
			t.Errorf("%q was accepted", presented)
		}
	}
	// A hash of the wrong length never matches, and never panics.
	for _, stored := range []string{"", "abc", strings.Repeat("0", 63), strings.Repeat("0", 65)} {
		if KeyMatches(key, stored) {
			t.Errorf("the key matched a stored hash of %d characters", len(stored))
		}
	}
}

func TestKeyStreamIDRefusesWhatIsNotAKey(t *testing.T) {
	for _, bad := range []string{"", "_", "nosuchseparator", "_secret", "id_", strings.Repeat("a", 300) + "_b"} {
		if id, ok := KeyStreamID(bad); ok {
			t.Errorf("%q was read as a key for stream %q", bad[:min(len(bad), 20)], id)
		}
	}
	// The id is everything before the first separator; the secret may not
	// contain one, but a client that sends one must not confuse the lookup.
	if id, ok := KeyStreamID("A_b_c"); !ok || id != "A" {
		t.Errorf("A_b_c read as %q, %v", id, ok)
	}
}

func TestFLVFraming(t *testing.T) {
	header := flvHeader(true)
	if !bytes.Equal(header, []byte{'F', 'L', 'V', 1, 0x05, 0, 0, 0, 9, 0, 0, 0, 0}) {
		t.Errorf("header with audio is % x", header)
	}
	// A silent stream must not promise audio: ffmpeg waits for every stream
	// the header announces before it starts.
	if flvHeader(false)[4] != 0x01 {
		t.Errorf("header without audio has flags %#x", flvHeader(false)[4])
	}

	body := []byte{0x17, 1, 0, 0, 0, 9, 9}
	const ts = 0x01020304 // past 24 bits: the top byte goes in the extension
	tag := flvTag(rtmp.TagVideo, ts, body)
	if tag[0] != 9 {
		t.Errorf("tag type %d", tag[0])
	}
	if size := int(tag[1])<<16 | int(tag[2])<<8 | int(tag[3]); size != len(body) {
		t.Errorf("data size %d, want %d", size, len(body))
	}
	if got := uint32(tag[7])<<24 | uint32(tag[4])<<16 | uint32(tag[5])<<8 | uint32(tag[6]); got != ts {
		t.Errorf("timestamp %#x, want %#x", got, ts)
	}
	if tag[8] != 0 || tag[9] != 0 || tag[10] != 0 {
		t.Error("stream id is not zero")
	}
	if !bytes.Equal(tag[11:11+len(body)], body) {
		t.Error("body changed")
	}
	if got := binary.BigEndian.Uint32(tag[len(tag)-4:]); got != uint32(11+len(body)) || len(tag) != 15+len(body) {
		t.Errorf("previous-tag-size %d in a tag of %d bytes", got, len(tag))
	}
}

func TestKeyframesAreRecognisedInBothTagLayouts(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"AVC keyframe", []byte{0x17, 1, 0, 0, 0}, true},
		{"AVC sequence header", []byte{0x17, 0, 0, 0, 0}, false}, // configuration, not a picture
		{"AVC inter frame", []byte{0x27, 1, 0, 0, 0}, false},
		{"a non-AVC keyframe", []byte{0x12, 0xff}, true},
		{"enhanced keyframe (HEVC coded frames)", []byte{0x91, 'h', 'v', 'c', '1'}, true},
		{"enhanced sequence start", []byte{0x90, 'h', 'v', 'c', '1'}, false},
		{"enhanced inter frame", []byte{0xa1, 'h', 'v', 'c', '1'}, false},
		{"empty", nil, false},
		{"one byte", []byte{0x17}, false},
	}
	for _, c := range cases {
		if got := isKeyframe(c.data); got != c.want {
			t.Errorf("%s: isKeyframe = %v, want %v", c.name, got, c.want)
		}
	}
}
