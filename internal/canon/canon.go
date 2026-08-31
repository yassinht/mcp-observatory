// Package canon produces a byte-stable serialization of a JSON value.
//
// Why this exists: an observatory must distinguish "the server changed its tool
// surface" from "the server serialized the same surface differently". A raw
// byte hash cannot -- key order, whitespace and tool ordering all vary between
// requests to the same unchanged server. Hashing raw bytes would report a
// change every single day and the log would be noise.
//
// Canonicalization here follows RFC 8785 (JCS) in spirit: object keys sorted by
// their UTF-16 code units, no insignificant whitespace, arrays left in order.
// Numbers are emitted as their original source literal rather than being routed
// through float64, so a schema containing 1e400 or a 20-digit integer survives
// a round trip instead of being silently rewritten.
package canon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Canonicalize parses src as JSON and returns its canonical form.
// It returns an error if src is not valid JSON -- callers observing a malformed
// response should archive the raw bytes and record the error, not discard it.
func Canonicalize(src []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber() // keep numeric literals verbatim
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canon: parse: %w", err)
	}
	// Reject trailing content: two concatenated JSON values are not one document.
	if dec.More() {
		return nil, fmt.Errorf("canon: trailing data after top-level value")
	}
	var buf bytes.Buffer
	if err := write(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func write(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		// Emit the literal exactly as the server wrote it.
		buf.WriteString(t.String())
	case string:
		return writeString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := write(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Sort(utf16Keys(keys))
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := write(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canon: unexpected type %T", v)
	}
	return nil
}

// writeString emits a JSON string escaped per RFC 8785 section 3.2.2.2:
// only the characters that MUST be escaped are escaped, using the short forms
// where they exist. Go's encoding/json additionally escapes <, > and & for HTML
// safety, which would make our output non-canonical, so we do it by hand.
func writeString(buf *bytes.Buffer, s string) error {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
	return nil
}

// utf16Keys sorts strings by UTF-16 code unit, which is what RFC 8785 requires
// and is NOT the same as Go's default byte-wise sort for characters outside the
// Basic Multilingual Plane. An emoji in a tool name would otherwise sort
// differently here than in a JCS implementation elsewhere, and the whole point
// of canonicalization is that independent implementations agree.
type utf16Keys []string

func (k utf16Keys) Len() int      { return len(k) }
func (k utf16Keys) Swap(i, j int) { k[i], k[j] = k[j], k[i] }
func (k utf16Keys) Less(i, j int) bool {
	a, b := utf16Of(k[i]), utf16Of(k[j])
	for n := 0; n < len(a) && n < len(b); n++ {
		if a[n] != b[n] {
			return a[n] < b[n]
		}
	}
	return len(a) < len(b)
}

func utf16Of(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for _, r := range s {
		if r <= 0xFFFF {
			out = append(out, uint16(r))
			continue
		}
		r -= 0x10000
		out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
	}
	return out
}
