package cli

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/state"
)

// The default view answers "what is running?": one row per repo, covering
// only its loaded extensions, with a summary line accounting for everything
// hidden — a NOT LOADED row must not disappear without trace.
func TestListDefaultShowsLoadedAndCountsTheRest(t *testing.T) {
	host := startFakeHost(t, idA, idB)
	host.mu.Lock()
	host.disabledInChrome = map[string]bool{idB: true}
	host.mu.Unlock()
	keyC, idC := fixtureKey("status-c")
	keyD, idD := fixtureKey("status-d")
	seedRepo(t, "tools",
		state.Extension{Dir: "a", Name: "Live", ID: idA, Key: keyA},
		state.Extension{Dir: "b", Name: "SwitchedOff", ID: idB, Key: keyB},
		state.Extension{Dir: "c", Name: "Missing", ID: idC, Key: keyC},
		state.Extension{Dir: "d", Name: "Idle", ID: idD, Key: keyD, Disabled: true},
	)

	out, err := run(t, "", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	// A real row, not a substring of the "Not shown: … not-loaded" summary.
	if !regexp.MustCompile(`(?m)^tools\s+branch:main\s+loaded\s*$`).MatchString(out) {
		t.Errorf("expected the repo row \"tools branch:main loaded\":\n%s", out)
	}
	for _, name := range []string{"Live", "SwitchedOff", "Missing", "Idle"} {
		if strings.Contains(out, name) {
			t.Errorf("extension names are --full territory, found %s:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "Not shown: 1 chrome-disabled, 1 not-loaded, 1 available. See: cepm list --all\n") {
		t.Errorf("hidden rows must be accounted for:\n%s", out)
	}

	// In --full mode the hint keeps the shape the user chose.
	full, err := run(t, "", "list", "--full")
	if err != nil {
		t.Fatalf("list --full: %v\n%s", err, full)
	}
	if !strings.Contains(full, "See: cepm list --all --full") {
		t.Errorf("the --full footer should suggest --all --full:\n%s", full)
	}
}

// Nothing loaded: the default explains itself rather than printing an empty
// table that reads as breakage.
func TestListDefaultNothingLoaded(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "a", Name: "Idle", ID: idA, Key: keyA, Disabled: true})
	out, err := run(t, "", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No loaded extensions.") ||
		!strings.Contains(out, "Not shown: 1 available. See: cepm list --all") {
		t.Errorf("expected the empty-default notice with the hidden count:\n%s", out)
	}
}

// With the host unreachable nothing is provably loaded; the default keeps
// the enabled rows visible as unknown instead of rendering an empty table
// that reads as "nothing is running".
func TestListDefaultShowsUnknownWhenHostGone(t *testing.T) {
	host := startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "a", Name: "Live", ID: idA, Key: keyA})
	host.stopListener()

	out, err := run(t, "", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "tools") || !strings.Contains(out, "unknown (host not connected)") {
		t.Errorf("enabled extensions should stay visible as unknown:\n%s", out)
	}
}

// The default columns are the ones a human scans — REPO, TRACK, STATUS, one
// row per repo; extension names, ids, dirs and URLs are --full territory.
func TestListDefaultColumnsAndFull(t *testing.T) {
	startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "subdir", Name: "MyExt", ID: idA, Key: keyA})

	out, err := run(t, "", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	// The positive anchor keeps the negative checks from passing vacuously
	// if the default view stopped rendering at all.
	if !regexp.MustCompile(`(?m)^tools\s+branch:main\s+loaded\s*$`).MatchString(out) {
		t.Fatalf("expected the aggregated repo row:\n%s", out)
	}
	for _, leak := range []string{"MyExt", idA, "example.com", "subdir"} {
		if strings.Contains(out, leak) {
			t.Errorf("default table should not carry %q:\n%s", leak, out)
		}
	}

	full, err := run(t, "", "list", "--full")
	if err != nil {
		t.Fatalf("list --full: %v\n%s", err, full)
	}
	for _, want := range []string{"MyExt", idA, "https://***@example.com/t/r.git", "subdir"} {
		if !strings.Contains(full, want) {
			t.Errorf("full table should carry %q:\n%s", want, full)
		}
	}
}

// --all renders the complete inventory — stale leftovers and orphans
// included — one row per extension, so the statuses it reveals are readable
// by name, with no summary left to print. Ids, dirs and URLs stay --full
// territory: --all answers "which and in what state", --full "identified
// how".
func TestListAllShowsEveryStatus(t *testing.T) {
	startFakeHost(t, idA)
	keyC, idC := fixtureKey("all-c")
	seedRepo(t, "tools",
		state.Extension{Dir: "a", Name: "Live", ID: idA, Key: keyA},
		state.Extension{Dir: "c", Name: "Idle", ID: idC, Key: keyC, Disabled: true},
	)
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Repos["tools"].AddStale(state.StaleExtension{ID: idB, Name: "Gone", Reason: "removed"})
	st.AddOrphans([]state.StaleExtension{{ID: idZ, Name: "Orphaned", Reason: "uninstalled"}})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "list", "--all")
	if err != nil {
		t.Fatalf("list --all: %v\n%s", err, out)
	}
	for _, want := range []string{"Live", "Idle", "Gone", "Orphaned",
		"(uninstalled)", "stale — run: cepm cleanup"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all should show %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{idA, "example.com"} {
		if strings.Contains(out, leak) {
			t.Errorf("--all without --full should not carry %q:\n%s", leak, out)
		}
	}
	if strings.Contains(out, "Not shown") {
		t.Errorf("--all hides nothing, so there is nothing to account for:\n%s", out)
	}

	full, err := run(t, "", "list", "--all", "--full")
	if err != nil {
		t.Fatalf("list --all --full: %v\n%s", err, full)
	}
	for _, want := range []string{"Live", "Idle", "Gone", "Orphaned", idA, idC} {
		if !strings.Contains(full, want) {
			t.Errorf("--all --full should show %q:\n%s", want, full)
		}
	}
}

// --status narrows to exactly the named statuses, and rejects words outside
// the vocabulary so a typo cannot read as "nothing matched".
func TestListStatusFilter(t *testing.T) {
	keyC, idC := fixtureKey("filter-c")
	keyD, idD := fixtureKey("filter-d")
	keyE, idE := fixtureKey("filter-e")
	host := startFakeHost(t, idA, idB, idE)
	host.mu.Lock()
	host.disabledInChrome = map[string]bool{idB: true}
	host.mu.Unlock()
	seedRepo(t, "tools",
		state.Extension{Dir: "a", Name: "Live", ID: idA, Key: keyA},
		state.Extension{Dir: "b", Name: "SwitchedOff", ID: idB, Key: keyB},
		state.Extension{Dir: "c", Name: "Missing", ID: idC, Key: keyC},
		state.Extension{Dir: "d", Name: "Idle", ID: idD, Key: keyD, Disabled: true},
		state.Extension{Dir: "e", Name: "Unmanaged", ID: idE, Key: keyE, Disabled: true},
	)

	names := []string{"Live", "SwitchedOff", "Missing", "Idle", "Unmanaged"}
	cases := []struct {
		status string
		want   []string
	}{
		{"loaded", []string{"Live"}},
		{"chrome-disabled", []string{"SwitchedOff"}},
		{"not-loaded", []string{"Missing"}},
		{"available", []string{"Idle"}},
		{"not-enabled", []string{"Unmanaged"}},
		{"loaded,available", []string{"Live", "Idle"}},
	}
	for _, tc := range cases {
		// No --full: an explicit selection expands to per-extension rows on
		// its own, so the selection is observable by name.
		out, err := run(t, "", "list", "--status", tc.status)
		if err != nil {
			t.Fatalf("list --status %s: %v\n%s", tc.status, err, out)
		}
		for _, name := range names {
			if got, want := strings.Contains(out, name), slices.Contains(tc.want, name); got != want {
				t.Errorf("--status %s: %s shown=%v, want %v\n%s", tc.status, name, got, want, out)
			}
		}
		if strings.Contains(out, "Not shown") {
			t.Errorf("--status %s: an explicit filter needs no summary:\n%s", tc.status, out)
		}
	}

	// --full composes with --status: the same selection, with the
	// identifying columns added.
	full, err := run(t, "", "list", "--status", "loaded", "--full")
	if err != nil {
		t.Fatalf("list --status loaded --full: %v\n%s", err, full)
	}
	for _, want := range []string{"Live", idA} {
		if !strings.Contains(full, want) {
			t.Errorf("--status loaded --full should carry %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "Missing") {
		t.Errorf("--status loaded --full must keep the selection:\n%s", full)
	}

	out, err := run(t, "", "list", "--status", "stale")
	if err != nil {
		t.Fatalf("list --status stale: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No extensions with status stale.") {
		t.Errorf("an explicit filter with no matches should say so:\n%s", out)
	}

	if _, err := run(t, "", "list", "--status", "laoded"); err == nil || !strings.Contains(err.Error(), "valid:") {
		t.Errorf("a typo must fail with the vocabulary, got: %v", err)
	}

	// --status "" (an empty $VAR in a script) must not silently degrade to
	// the default view.
	if _, err := run(t, "", "list", "--status", ""); err == nil || !strings.Contains(err.Error(), "valid:") {
		t.Errorf("an empty --status must fail with the vocabulary, got: %v", err)
	}
}

// A registered repository can be left with no extensions at all (every dir
// removed upstream, then cleaned up); --all must say so rather than print
// nothing and read like an unregistered state.
func TestListAllExtensionlessRepoSaysSo(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools")

	out, err := run(t, "", "list", "--all")
	if err != nil {
		t.Fatalf("list --all: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No extensions registered.") {
		t.Errorf("--all on an extensionless inventory should say so:\n%s", out)
	}
}

// An explicit JSON filter narrows rows, but never hides that a repo's
// updates are failing — the JSON counterpart of the table's ⚠ line.
func TestListJSONStatusKeepsFailingRepo(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "a", Name: "Idle", ID: idA, Key: keyA, Disabled: true})
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Repos["tools"].LastError = "fatal: could not read from remote"
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "list", "--json", "--status", "loaded")
	if err != nil {
		t.Fatalf("list --json --status loaded: %v\n%s", err, out)
	}
	var p struct {
		Repos []struct {
			Name       string `json:"name"`
			LastError  string `json:"lastError"`
			Extensions []any  `json:"extensions"`
		} `json:"repos"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(p.Repos) != 1 || p.Repos[0].LastError == "" || len(p.Repos[0].Extensions) != 0 {
		t.Errorf("the failing repo should survive the filter with its lastError (and no rows), got %+v", p)
	}
}

// JSON is for scripts: complete by default (every status, plus statusKey for
// the stable vocabulary), narrowed only by an explicit --status.
func TestListJSONStatusKeyAndFilter(t *testing.T) {
	startFakeHost(t, idA)
	keyC, idC := fixtureKey("json-c")
	seedRepo(t, "tools",
		state.Extension{Dir: "a", Name: "Live", ID: idA, Key: keyA},
		state.Extension{Dir: "c", Name: "Missing", ID: idC, Key: keyC},
	)

	type payload struct {
		Repos []struct {
			Name       string `json:"name"`
			Extensions []struct {
				Name      string `json:"name"`
				StatusKey string `json:"statusKey"`
			} `json:"extensions"`
		} `json:"repos"`
	}
	decode := func(args ...string) payload {
		t.Helper()
		out, err := run(t, "", args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		var p payload
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			t.Fatalf("%v is not valid JSON: %v\n%s", args, err, out)
		}
		return p
	}

	p := decode("list", "--json")
	if len(p.Repos) != 1 || len(p.Repos[0].Extensions) != 2 {
		t.Fatalf("default JSON must be complete, got %+v", p)
	}
	keys := map[string]string{}
	for _, e := range p.Repos[0].Extensions {
		keys[e.Name] = e.StatusKey
	}
	if keys["Live"] != "loaded" || keys["Missing"] != "not-loaded" {
		t.Errorf("statusKey mismatch: %v", keys)
	}

	p = decode("list", "--json", "--status", "not-loaded")
	if len(p.Repos) != 1 || len(p.Repos[0].Extensions) != 1 || p.Repos[0].Extensions[0].Name != "Missing" {
		t.Errorf("--status should narrow the JSON, got %+v", p)
	}

	if p = decode("list", "--json", "--status", "available"); len(p.Repos) != 0 {
		t.Errorf("a repo with no matching rows is not a match, got %+v", p)
	}
}
