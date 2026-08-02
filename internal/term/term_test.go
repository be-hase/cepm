package term

import (
	"strings"
	"testing"
)

func TestQuoteKeepsValueAsOneShellWord(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{`release;touch${IFS}/tmp/pwn`, `'release;touch${IFS}/tmp/pwn'`},
		{"it's", `'it'\''s'`},
		{"$(id)", "'$(id)'"},
	}
	for _, tt := range tests {
		if got := Quote(tt.in); got != tt.want {
			t.Errorf("Quote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Shell quoting stops a value from becoming a second command; it does nothing
// about one that repaints the terminal. Quoted output reaches a terminal, so
// it must not carry control characters either.
func TestQuoteEscapesControlCharacters(t *testing.T) {
	for _, in := range []string{"a\nb", "a\x1b[2Kb", "a\rb", "a\x00b"} {
		got := Quote(in)
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("Quote(%q) = %q, which still contains a control character", in, got)
				break
			}
		}
	}
}

func TestSafe(t *testing.T) {
	if got := Safe("a\nb"); got != `a\x0ab` {
		t.Errorf("Safe = %q", got)
	}
	if got := Safe("tab\there"); got != "tab here" {
		t.Errorf("Safe should turn a tab into a space, got %q", got)
	}
	if got := Safe("plain/path"); got != "plain/path" {
		t.Errorf("Safe should leave ordinary text alone, got %q", got)
	}
}

func TestSafeLines(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// Newlines survive but every continuation line is visibly quoted:
		// a remote-forged line cannot pose as a fresh cepm line.
		{"fatal: no access\nPlease check your keys", "fatal: no access\n  | Please check your keys"},
		{"fatal: gone\n✔ reloaded evil", "fatal: gone\n  | ✔ reloaded evil"},
		{"remote: \x1b]0;pwned\x07 done", `remote: \x1b]0;pwned\x07 done`},
		{"line one\r\nline two", "line one\n  | line two"},
		{"cr\rmid", `cr\x0dmid`},
		// Idempotent: a second pass changes nothing.
		{`remote: \x1b]0;pwned\x07`, `remote: \x1b]0;pwned\x07`},
		{"a\n  | already quoted", "a\n  | already quoted"},
	}
	for _, tt := range tests {
		if got := SafeLines(tt.in); got != tt.want {
			t.Errorf("SafeLines(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if again := SafeLines(SafeLines(tt.in)); again != tt.want {
			t.Errorf("SafeLines is not idempotent on %q: %q", tt.in, again)
		}
	}
}

// The full Unicode Bidi_Control set: every one of these can reorder or mask
// how a line reads, so every one must be caught and defanged.
func TestBidiControlIsCoveredCompletely(t *testing.T) {
	bidi := []rune{0x061C, 0x200E, 0x200F, 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069}
	for _, r := range bidi {
		s := "a" + string(r) + "b"
		if !HasControl(s) {
			t.Errorf("HasControl misses U+%04X", r)
		}
		if got := Safe(s); strings.ContainsRune(got, r) {
			t.Errorf("Safe leaves U+%04X intact: %q", r, got)
		}
		if got := Strip(s); strings.ContainsRune(got, r) {
			t.Errorf("Strip leaves U+%04X intact: %q", r, got)
		}
	}
}

func TestHasControl(t *testing.T) {
	for _, s := range []string{"a\nb", "\x1b", "a\u202eb"} {
		if !HasControl(s) {
			t.Errorf("HasControl(%q) = false", s)
		}
	}
	for _, s := range []string{"plain", "ext/sub", "日本語"} {
		if HasControl(s) {
			t.Errorf("HasControl(%q) = true", s)
		}
	}
}
