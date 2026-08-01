package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/nmhost"
)

// Stale/orphan records are the removal set the native host is willing to
// relay to Chrome, so they get the same self-certification live extensions
// do: a hand-edited state cannot smuggle a well-formed but foreign id into
// the authorized set, and even a genuine record only authorizes removal —
// reload stays reserved for live, enabled extensions.
func TestOrphanRecordsCannotSmuggleForeignIDs(t *testing.T) {
	startFakeHost(t)
	// A well-formed id whose claimed derivation does not produce it.
	writeRawState(t, fmt.Sprintf(`{"version":5,"repos":{},
      "orphans":[{"id":%q,"name":"X","reason":"uninstalled","srcRepo":"tools","srcDir":"ext"}]}`, idA))
	if _, err := nmhost.AuthorizeRemoval([]string{idA}); err == nil {
		t.Error("an underivable orphan id must not authorize a removal")
	}

	// The same id with its real derivation is a genuine record: removal is
	// authorized, reload is not.
	writeRawState(t, fmt.Sprintf(`{"version":5,"repos":{},
      "orphans":[{"id":%q,"name":"X","reason":"uninstalled","srcRepo":"tools","srcDir":"ext","srcKey":%q}]}`, idA, keyA))
	if _, err := nmhost.AuthorizeRemoval([]string{idA}); err != nil {
		t.Errorf("a self-certified orphan must be removable: %v", err)
	}
	if _, err := nmhost.AuthorizeReload([]string{idA}); err == nil {
		t.Error("an orphan is not a live extension; reload must be refused")
	}
}

// list renders broken states too (it is how you inspect them), and LastError
// carries git/ssh stderr even in valid states: neither may reach the
// terminal with escape sequences intact.
func TestListAndDoctorNeverPrintControlCharactersFromState(t *testing.T) {
	startFakeHost(t)
	// An invalid state (ESC in branch) plus a LastError with an OSC
	// sequence — list must show both, defanged.
	writeRawState(t, fmt.Sprintf(`{"version":5,"repos":{
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
	writeRawState(t, fmt.Sprintf(`{"version":5,"repos":{
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

// A clone without a state entry (an install killed between the rename and
// the save) must not be met with "run cepm uninstall": uninstall does not
// know the name, and the old advice was a dead end.
func TestInstallNamesTheRecoveryForAnUnregisteredClone(t *testing.T) {
	startFakeHost(t)
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = run(t, "", "install", "https://example.com/t/tools.git", "--name", "tools")
	if err == nil {
		t.Fatal("install should refuse the occupied directory")
	}
	if strings.Contains(err.Error(), "cepm uninstall") {
		t.Errorf("must not advise uninstall for an unregistered clone: %v", err)
	}
	if !strings.Contains(err.Error(), "interrupted install") {
		t.Errorf("should explain what this probably is: %v", err)
	}
}
