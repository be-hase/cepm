package cli

import (
	"fmt"
	"strings"
	"testing"
)

// list renders broken states too (it is how you inspect them), and LastError
// carries git/ssh stderr even in valid states: neither may reach the
// terminal with escape sequences intact.
func TestListAndDoctorNeverPrintControlCharactersFromState(t *testing.T) {
	startFakeHost(t)
	// An invalid state (ESC in branch) plus a LastError with an OSC
	// sequence — list must show both, defanged.
	writeRawState(t, fmt.Sprintf(`{"version":6,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main\u001b[2K","head":"%s",
               "lastError":"remote: \u001b]0;pwned\u0007 fatal",
               "extensions":[{"dir":"ext","name":"Ext","id":%q,"key":%q}]}}}`,
		strings.Repeat("a", 40), idA, keyA))

	listOut, _ := run(t, "", "list")
	docOut, _ := run(t, "", "doctor")
	for label, s := range map[string]string{"list": listOut, "doctor": docOut} {
		if strings.Contains(s, "\x1b") || strings.Contains(s, "\x07") {
			t.Errorf("%s output contains a raw escape sequence:\n%q", label, s)
		}
	}

	// And a *valid* state whose LastError carries the sequences: this is the
	// everyday case (a flaky remote), and doctor renders it too.
	writeRawState(t, fmt.Sprintf(`{"version":6,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"%s",
               "lastError":"remote: \u001b]0;pwned\u0007 fatal",
               "extensions":[{"dir":"ext","name":"Ext","id":%q,"key":%q}]}}}`,
		strings.Repeat("a", 40), idA, keyA))
	listOut, _ = run(t, "", "list")
	docOut, _ = run(t, "", "doctor")
	for label, s := range map[string]string{"list": listOut, "doctor": docOut} {
		if !strings.Contains(s, "pwned") {
			continue // this command does not render LastError; nothing to defang
		}
		if strings.Contains(s, "\x1b") || strings.Contains(s, "\x07") {
			t.Errorf("%s output contains a raw escape sequence from LastError:\n%q", label, s)
		}
	}
	if !strings.Contains(listOut, "pwned") {
		t.Error("list should render the last error (defanged) — the fixture missed its target")
	}
}
