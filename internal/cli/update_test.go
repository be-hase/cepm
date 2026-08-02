package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/updater"
)

// A failed update prints the error live (unlike list/doctor, which render the
// stored, stripped LastError), and that error carries git/SSH stderr. gitx
// escapes at the source; this pins the display layer so a differently-sourced
// error cannot repaint the terminal either.
func TestUpdateFailuresNeverPrintControlCharacters(t *testing.T) {
	var out strings.Builder
	printUpdateResults(&out, []updater.RepoResult{{
		Name: "tools",
		Err:  errors.New("fetch failed: remote: \x1b]0;pwned\x07\nfatal: gone"),
	}})
	s := out.String()
	if strings.Contains(s, "\x1b") || strings.Contains(s, "\x07") {
		t.Errorf("update output contains a raw escape sequence:\n%q", s)
	}
	if !strings.Contains(s, "pwned") {
		t.Errorf("the error text should still be shown (defanged):\n%q", s)
	}
}
