package archive

import (
	"bytes"
	"encoding/json"
)

// WithoutLegacyVersion returns data with one exact obsolete top-level
// `"version": 1` object member removed, byte-for-byte otherwise unchanged.
//
// Older AgentReel writers stamped that member on persisted recording state and
// archive headers. It is inert legacy metadata: nothing reads its value, it
// never selects a format, migration, negotiation or compatibility path, and
// current writers do not emit it. Removing it lets the unversioned strict
// decoders accept those existing recordings unchanged. Any other unknown key,
// any other version value or spelling (`2`, `"1"`, `1.0`), a nested member or
// a second `version` member is left in place for strict decoding to reject.
// Malformed input stays malformed and is reported by the strict decoder.
func WithoutLegacyVersion(data []byte) []byte {
	return WithoutLegacyMember(data, "version", "1")
}

// WithoutLegacyMember returns data with one exact obsolete top-level object
// member `key` whose value is the exact JSON token `value` removed,
// byte-for-byte otherwise unchanged. It is the generalization behind
// WithoutLegacyVersion and follows the same rules: the value must match
// literally (no numeric or string equivalence), only a top-level member is
// considered, only the first matching member is removed so a duplicate stays
// for strict rejection, and malformed or non-object input is returned as is.
func WithoutLegacyMember(data []byte, key, value string) []byte {
	return topLevelMember(data, key, func(first bool, start, _, end int, raw []byte) []byte {
		if string(bytes.TrimSpace(raw)) != value {
			return nil
		}
		rest := data[end:]
		if first {
			if trimmed := bytes.TrimLeft(rest, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == ',' {
				rest = trimmed[1:]
			}
		}
		out := make([]byte, 0, len(data))
		out = append(out, data[:start]...)
		return append(out, rest...)
	})
}

// topLevelMember walks the members of a top-level JSON object and calls visit
// for each member named key with the byte offsets of the separator-or-key
// start, the value start and the value end plus the raw value. A non-nil visit
// result is returned immediately; nil keeps scanning. Any other input, decode
// error or unmatched key returns data unchanged.
func topLevelMember(data []byte, key string, visit func(first bool, start, valueStart, end int, raw []byte) []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, e := dec.Token(); e != nil || tok != json.Delim('{') {
		return data
	}
	first := true
	for dec.More() {
		start := int(dec.InputOffset()) // the separating comma, or the first key
		k, e := dec.Token()
		if e != nil {
			return data
		}
		var raw json.RawMessage
		if e = dec.Decode(&raw); e != nil {
			return data
		}
		end := int(dec.InputOffset())
		if s, ok := k.(string); ok && s == key {
			if out := visit(first, start, end-len(raw), end, raw); out != nil {
				return out
			}
		}
		first = false
	}
	return data
}
