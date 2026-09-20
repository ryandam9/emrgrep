package scan

import (
	"unicode/utf8"
)

// sanitizePlaceholder replaces control characters and invalid bytes in
// printed output, preventing terminal-escape injection and NUL
// confusion (H-09).
const sanitizePlaceholder = '?'

// isDangerousRune reports whether r must not reach a terminal: C0
// controls other than tab, DEL, C1 controls (CSI/OSC and friends), and
// Unicode characters that reorder, hide, or split output — bidi marks,
// embeddings, overrides and isolates, zero-width and joiner
// characters, invisible math operators, interlinear annotation
// anchors, the BOM/ZWNBSP, and line/paragraph separators.
func isDangerousRune(r rune) bool {
	switch {
	case r == '\t':
		return false
	case r < 0x20 || r == 0x7f: // C0 + DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1
		return true
	case r == 0x061c: // Arabic letter mark (invisible bidi control)
		return true
	case r == 0x180e: // Mongolian vowel separator (zero-width)
		return true
	case r >= 0x200b && r <= 0x200f: // zero-width, ZWJ/ZWNJ, LRM/RLM
		return true
	case r == 0x2028 || r == 0x2029: // line/paragraph separators
		return true
	case r >= 0x202a && r <= 0x202e: // bidi embedding/override
		return true
	case r >= 0x2060 && r <= 0x2064: // word joiner + invisible operators
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	case r >= 0xfff9 && r <= 0xfffb: // interlinear annotation anchors
		return true
	case r == 0xfeff: // BOM / zero-width no-break space
		return true
	}
	return false
}

// clean reports whether b needs no sanitization: the common case of
// plain ASCII (or valid UTF-8) free of dangerous characters.
func clean(b []byte) bool {
	for i := 0; i < len(b); {
		c := b[i]
		if c < utf8.RuneSelf {
			if isDangerousRune(rune(c)) {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return false // invalid byte
		}
		if isDangerousRune(r) {
			return false
		}
		i += size
	}
	return true
}

// Sanitize replaces every dangerous character (see isDangerousRune)
// and every invalid UTF-8 byte with a placeholder. Applied by default
// to matched content, object keys, and ZIP entry names before printing
// (§7.2). It never mutates its input.
func Sanitize(b []byte) []byte {
	if clean(b) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c < utf8.RuneSelf {
			if isDangerousRune(rune(c)) {
				out = append(out, sanitizePlaceholder)
			} else {
				out = append(out, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if (r == utf8.RuneError && size == 1) || isDangerousRune(r) {
			out = append(out, sanitizePlaceholder)
		} else {
			out = append(out, b[i:i+size]...)
		}
		i += size
	}
	return out
}

// SanitizeString is Sanitize for strings.
func SanitizeString(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	out := Sanitize(b)
	if len(out) == len(b) && &out[0] == &b[0] {
		return s
	}
	return string(out)
}
