// Package term prepares values for the terminal.
//
// Much of what cepm prints comes from a repository: branch names, extension
// directories, manifest names. Two things must never happen with such values:
// they must not become extra commands when a user copies a suggested command,
// and they must not forge lines or hide text with control characters.
package term

import (
	"strings"
	"unicode"
)

// Quote renders s as a single POSIX shell word, so a value with spaces,
// semicolons or $(...) stays one argument when pasted into a shell.
//
// Control characters are escaped rather than quoted through: shell quoting
// stops a value from becoming a second command, but does nothing about a
// value that repaints the terminal. cepm rejects such names where it can, so
// a quoted value that had to be escaped here is already one no command should
// be run against — better to show it visibly broken than to emit it raw.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(Safe(s), "'", `'\''`) + "'"
}

// Safe makes s printable: control characters (newlines, carriage returns,
// escape sequences) are replaced with a visible form so a value cannot forge
// a table row, blank a line, or dress itself up with colour.
func Safe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			b.WriteString("\\x")
			const hex = "0123456789abcdef"
			b.WriteByte(hex[(r>>4)&0xF])
			b.WriteByte(hex[r&0xF])
		case isBidi(r):
			// Bidirectional overrides can visually reorder a line.
			b.WriteString("\\u200e")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isBidi reports whether r can reorder how a line is displayed.
func isBidi(r rune) bool {
	return (r >= 0x200E && r <= 0x200F) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// Strip removes every character Safe would have to escape, for values cepm
// may rewrite — a manifest name is a label, so dropping the dangerous parts
// is friendlier than showing escapes. Paths cannot use this: they have to
// keep matching the filesystem.
func Strip(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || isBidi(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// HasControl reports whether s contains characters that have no place in a
// value cepm will print or pass around.
func HasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || isBidi(r) {
			return true
		}
	}
	return false
}
