package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Repository URLs reach the terminal, the log file and doctor output, which
// people paste into chats — a token in one of them must never survive.
func TestRedactURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://user:TOKEN@ghe.example.com/t/r.git", "https://***@ghe.example.com/t/r.git"},
		{"https://ghp_abcdef123@github.com/t/r.git", "https://***@github.com/t/r.git"},
		{"https://x-access-token:ghs_zzz@github.com/t/r", "https://***@github.com/t/r"},
		{"https://github.com/t/r.git", "https://github.com/t/r.git"},
		{"git@github.com:t/r.git", "git@github.com:t/r.git"}, // no credentials
		{
			"clone https://u:p@h/x.git failed: fatal: could not read from https://u:p@h/x.git",
			"clone https://***@h/x.git failed: fatal: could not read from https://***@h/x.git",
		},
	}
	for _, tt := range tests {
		if got := RedactURL(tt.in); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Git passes SSH's stderr through verbatim, and errors from this package
// reach the terminal — so a hostile server's escape-laden banner must come
// back defanged. A fake SSH stands in for that server; git's own path
// quoting never fires here, so the raw bytes really do reach run().
func TestErrorsCarryNoRawControlCharacters(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-ssh")
	banner := "#!/bin/sh\nprintf 'remote: \\033]0;pwned\\007 fatal\\n' >&2\nexit 128\n"
	if err := os.WriteFile(script, []byte(banner), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", script)

	err := Clone(context.Background(), "ssh://example.invalid/repo.git", filepath.Join(dir, "dst"), "")
	if err == nil {
		t.Fatal("clone through the failing fake SSH should fail")
	}
	msg := err.Error()
	if strings.ContainsRune(msg, 0x1b) || strings.ContainsRune(msg, 0x07) {
		t.Errorf("error carries a raw escape sequence: %q", msg)
	}
	if !strings.Contains(msg, "pwned") {
		t.Errorf("fixture missed: the banner text should appear in %q", msg)
	}
	if !strings.Contains(msg, `\x1b`) {
		t.Errorf("the escaped form should appear in %q", msg)
	}
}
