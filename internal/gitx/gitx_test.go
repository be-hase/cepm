package gitx

import "testing"

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
