package rtmp

import (
	"encoding/binary"
	"errors"
	"math"
)

// AMF0 is the serialisation RTMP commands are written in. This is the subset
// a publisher's commands actually use; it reads every type that can legally
// appear so that an unfamiliar field in a `connect` object is skipped rather
// than derailing the parse, and writes only the four types a reply needs.

const (
	amfNumber      = 0x00
	amfBoolean     = 0x01
	amfString      = 0x02
	amfObject      = 0x03
	amfNull        = 0x05
	amfUndefined   = 0x06
	amfECMAArray   = 0x08
	amfObjectEnd   = 0x09
	amfStrictArray = 0x0a
	amfDate        = 0x0b
	amfLongString  = 0x0c
)

var errAMF = errors.New("malformed AMF0")

// amfDepth bounds nesting. A command is two levels deep at most; the bound is
// what stops a crafted message recursing until the stack is gone.
const amfDepth = 8

// amfDecode reads values until the buffer is empty.
func amfDecode(b []byte) ([]any, error) {
	var out []any
	for len(b) > 0 {
		v, rest, err := amfValue(b, 0)
		if err != nil {
			return out, err
		}
		out, b = append(out, v), rest
	}
	return out, nil
}

func amfValue(b []byte, depth int) (any, []byte, error) {
	if len(b) < 1 || depth > amfDepth {
		return nil, nil, errAMF
	}
	kind, b := b[0], b[1:]
	switch kind {
	case amfNumber:
		if len(b) < 8 {
			return nil, nil, errAMF
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[8:], nil
	case amfBoolean:
		if len(b) < 1 {
			return nil, nil, errAMF
		}
		return b[0] != 0, b[1:], nil
	case amfString:
		return amfShortString(b)
	case amfLongString:
		if len(b) < 4 {
			return nil, nil, errAMF
		}
		n := int(binary.BigEndian.Uint32(b))
		if n < 0 || len(b)-4 < n {
			return nil, nil, errAMF
		}
		return string(b[4 : 4+n]), b[4+n:], nil
	case amfNull, amfUndefined:
		return nil, b, nil
	case amfECMAArray:
		if len(b) < 4 {
			return nil, nil, errAMF
		}
		// The count is advisory and routinely wrong; the end marker is what
		// ends it, exactly as for an object.
		return amfProperties(b[4:], depth)
	case amfObject:
		return amfProperties(b, depth)
	case amfStrictArray:
		if len(b) < 4 {
			return nil, nil, errAMF
		}
		n := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		if n < 0 || n > len(b) {
			// Every element is at least one byte, so a count larger than what
			// is left is a lie, and believing it would allocate for it.
			return nil, nil, errAMF
		}
		items := make([]any, 0, n)
		for i := 0; i < n; i++ {
			v, rest, err := amfValue(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			items, b = append(items, v), rest
		}
		return items, b, nil
	case amfDate:
		if len(b) < 10 {
			return nil, nil, errAMF
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[10:], nil
	default:
		return nil, nil, errAMF
	}
}

func amfShortString(b []byte) (string, []byte, error) {
	if len(b) < 2 {
		return "", nil, errAMF
	}
	n := int(binary.BigEndian.Uint16(b))
	if len(b)-2 < n {
		return "", nil, errAMF
	}
	return string(b[2 : 2+n]), b[2+n:], nil
}

func amfProperties(b []byte, depth int) (map[string]any, []byte, error) {
	obj := map[string]any{}
	for {
		key, rest, err := amfShortString(b)
		if err != nil {
			return nil, nil, err
		}
		if key == "" {
			if len(rest) < 1 || rest[0] != amfObjectEnd {
				return nil, nil, errAMF
			}
			return obj, rest[1:], nil
		}
		v, rest, err := amfValue(rest, depth+1)
		if err != nil {
			return nil, nil, err
		}
		obj[key], b = v, rest
	}
}

// amfField is one property of an object being written. A slice of these, not
// a map: the order a reply's properties are written in is fixed, so that the
// bytes on the wire are the same every time and a test can compare them.
type amfField struct {
	Key   string
	Value any
}

// amfEncode writes values. It knows float64, string, nil and []amfField, which
// is everything a reply to a publisher contains.
func amfEncode(values ...any) []byte {
	var b []byte
	for _, v := range values {
		b = amfAppend(b, v)
	}
	return b
}

func amfAppend(b []byte, v any) []byte {
	switch x := v.(type) {
	case float64:
		b = append(b, amfNumber)
		return binary.BigEndian.AppendUint64(b, math.Float64bits(x))
	case string:
		b = append(b, amfString)
		b = binary.BigEndian.AppendUint16(b, uint16(len(x)))
		return append(b, x...)
	case []amfField:
		b = append(b, amfObject)
		for _, f := range x {
			b = binary.BigEndian.AppendUint16(b, uint16(len(f.Key)))
			b = append(b, f.Key...)
			b = amfAppend(b, f.Value)
		}
		return append(b, 0, 0, amfObjectEnd)
	default:
		return append(b, amfNull)
	}
}
