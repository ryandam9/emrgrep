package scan

import (
	"bytes"
	"strings"
	"testing"
)

func TestSanitizeControlChars(t *testing.T) {
	in := []byte("a\x1b[31mred\x00b\tc")
	out := Sanitize(in)
	want := "a?[31mred?b\tc"
	if string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
	// Input must not be mutated.
	if !bytes.Contains(in, []byte{0x1b}) {
		t.Fatal("Sanitize mutated its input")
	}
}

func TestSanitizeCleanPassthrough(t *testing.T) {
	in := []byte("plain text with tab\tand unicode: héllo")
	if out := Sanitize(in); &out[0] != &in[0] {
		t.Fatal("clean input should be returned without copying")
	}
}

func TestSanitizeString(t *testing.T) {
	if got := SanitizeString("key\rwith\ncontrols"); got != "key?with?controls" {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeString("clean-key.gz"); got != "clean-key.gz" {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeString(""); got != "" {
		t.Fatalf("empty string: %q", got)
	}
}

func TestSanitizeDEL(t *testing.T) {
	if got := Sanitize([]byte{'a', 0x7f, 'b'}); string(got) != "a?b" {
		t.Fatalf("DEL not sanitized: %q", got)
	}
}

// M-07: C1 controls, both as raw bytes (invalid UTF-8) and as their
// UTF-8 encodings, must not reach the terminal.
func TestSanitizeC1Controls(t *testing.T) {
	// Raw CSI byte 0x9b: invalid UTF-8 -> replaced.
	if got := Sanitize([]byte{'a', 0x9b, '3', '1', 'm', 'b'}); string(got) != "a?31mb" {
		t.Fatalf("raw C1 CSI: %q", got)
	}
	// UTF-8-encoded U+009B (0xC2 0x9B) -> one placeholder.
	if got := Sanitize([]byte("a\u009bb")); string(got) != "a?b" {
		t.Fatalf("encoded C1 CSI: %q", got)
	}
}

func TestSanitizeUnicodeControls(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\u202eevil\u202cb", "a?evil?b"}, // RLO + PDF bidi overrides
		{"x\u200by", "x?y"},                // zero-width space
		{"j\u200dq", "j?q"},                // zero-width joiner
		{"p\u2028q", "p?q"},                // line separator
		{"p\u2029q", "p?q"},                // paragraph separator
		{"m\u2066n\u2069o", "m?n?o"},       // bidi isolates
		{"bom\ufeffend", "bom?end"},        // BOM / ZWNBSP
	}
	for _, tc := range cases {
		if got := SanitizeString(tc.in); got != tc.want {
			t.Errorf("SanitizeString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeInvalidUTF8(t *testing.T) {
	in := []byte{'o', 'k', 0xff, 0xfe, 'x'}
	if got := Sanitize(in); string(got) != "ok??x" {
		t.Fatalf("invalid bytes: %q", got)
	}
}

func TestSanitizeUnicodePassthrough(t *testing.T) {
	for _, s := range []string{"héllo wörld", "日本語のログ", "emoji ✅ fine"} {
		if got := SanitizeString(s); got != s {
			t.Errorf("valid text mangled: %q -> %q", s, got)
		}
	}
}

// Invisible formatting characters that reorder or hide output must be
// replaced, not just the bidi and zero-width ranges. A log line can
// carry any of these, and a terminal renders them as nothing at all —
// which is exactly what makes them worth neutralizing.
func TestSanitizeInvisibleFormatting(t *testing.T) {
	cases := []struct{ name, in string }{
		{"word joiner", "before⁠after"},
		{"invisible times", "a⁢b"},
		{"invisible plus", "a⁤b"},
		{"arabic letter mark", "x؜y"},
		{"mongolian vowel separator", "x᠎y"},
		{"interlinear anchor", "x￹y"},
		{"interlinear terminator", "x￻y"},
	}
	for _, tc := range cases {
		got := SanitizeString(tc.in)
		if strings.ContainsAny(got, "⁠⁢⁤؜᠎￹￻") {
			t.Errorf("%s: %q survived sanitization as %q", tc.name, tc.in, got)
		}
		if !strings.Contains(got, "?") {
			t.Errorf("%s: expected a placeholder in %q", tc.name, got)
		}
	}
}

// Ordinary text — including non-ASCII that is perfectly safe to print
// — must pass through untouched.
func TestSanitizeLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"plain ascii",
		"tabs\tare\tfine",
		"café — naïve — 日本語",
		"emoji 🎯 and maths ∑∫",
	} {
		if got := SanitizeString(s); got != s {
			t.Errorf("SanitizeString(%q) = %q, want unchanged", s, got)
		}
	}
}
