package store

// NormalizeEventPayload returns payload with every NUL byte it represents
// replaced by U+FFFD, written as the 6-byte \ufffd escape. The pg profile
// stores event payloads in a JSONB column and JSONB cannot represent U+0000
// (PostgreSQL rejects the \u0000 escape with SQLSTATE 22P05 "unsupported
// Unicode escape sequence"), so both drivers normalize at the write
// boundary and the decoded payload stays equal across engines.
//
// The scanner is string-aware:
//   - inside strings, the \u0000 escape and a raw 0x00 byte become \ufffd;
//     any other raw control byte (< 0x20) becomes its \u00XX escape;
//   - all other \-escapes and non-ASCII bytes are copied verbatim;
//   - outside strings, structure and JSON whitespace are copied verbatim.
//
// Payloads carrying neither a NUL nor raw control bytes (protojson output is
// exactly that) are returned byte-identical, so sqlite's exact-protojson-text
// property survives for ordinary payloads. Applying it twice equals applying
// it once.
func NormalizeEventPayload(payload []byte) []byte {
	out := make([]byte, 0, len(payload))
	inString := false
	escaped := false
	for i := 0; i < len(payload); i++ {
		b := payload[i]
		if !inString {
			out = append(out, b)
			if b == '"' {
				inString = true
			}
			continue
		}
		if escaped {
			escaped = false
			// \u0000 -> \ufffd; the four hex digits are consumed. Any other
			// escape pair is copied verbatim (the escaped bytes after 'u'
			// are re-scanned, but they are ≥ 0x20 and neither '"' nor '\',
			// so they pass through unchanged).
			if b == 'u' && i+4 < len(payload) &&
				payload[i+1] == '0' && payload[i+2] == '0' &&
				payload[i+3] == '0' && payload[i+4] == '0' {
				out = append(out, "ufffd"...)
				i += 4
				continue
			}
			out = append(out, b)
			continue
		}
		switch {
		case b == '"':
			inString = false
			out = append(out, b)
		case b == '\\':
			escaped = true
			out = append(out, b)
		case b == 0x00:
			out = append(out, `\ufffd`...)
		case b < 0x20:
			// Raw control byte inside a string: escape it so the document
			// stays strict JSON; 0x00 still becomes the U+FFFD replacement.
			out = append(out, '\\', 'u', '0', '0',
				hexDigit(b>>4), hexDigit(b&0x0f))
		default:
			out = append(out, b)
		}
	}
	return out
}

const hexDigits = "0123456789abcdef"

func hexDigit(b byte) byte { return hexDigits[b&0x0f] }
