package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/nmhost"
	"github.com/be-hase/cepm/internal/nmmanifest"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
)

// fakeHost stands in for the native messaging host: it serves the control
// socket the CLI talks to, so command-level tests exercise the real IPC path
// (which is where a broken call order shows up).
type fakeHost struct {
	mu          sync.Mutex
	loaded      map[string]bool // extensions Chrome currently has
	uninstalled []string        // ids the CLI asked to remove
	reloaded    []string
	// reposAtUninstall records how many repositories were still registered
	// when a removal request arrived, which is what pins the ordering.
	reposAtUninstall []int
	// onUninstall runs while a removal request is being handled — the moment
	// the real Chrome would be showing its confirmation dialog.
	onUninstall func()
}

func startFakeHost(t *testing.T, loaded ...string) *fakeHost {
	t.Helper()
	// Short path: Unix sockets have a ~104 byte limit.
	dir, err := os.MkdirTemp("/tmp", "cepm-cli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}

	h := &fakeHost{loaded: map[string]bool{}}
	for _, id := range loaded {
		h.loaded[id] = true
	}
	sock, err := paths.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	l, err := ipc.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); l.Close() })
	go ipc.Serve(ctx, l, h.handle)
	return h
}

func (h *fakeHost) handle(_ context.Context, req ipc.Request) ipc.Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch req.Cmd {
	case ipc.CmdListChrome:
		var exts []ipc.ChromeExt
		for id := range h.loaded {
			exts = append(exts, ipc.ChromeExt{ID: id, Name: "Ext " + id, Version: "1.0", Enabled: true})
		}
		return ipc.Response{OK: true, Extensions: exts}
	case ipc.CmdUninstall:
		// Enforce the real authorization rule, not a stub: without this the
		// test would pass even if a command asked to remove an extension it
		// had already unregistered — the exact bug these tests exist for.
		if _, err := nmhost.ManagedIDs([]string{req.ID}); err != nil {
			return ipc.Response{Error: err.Error()}
		}
		if st, err := state.Load(); err == nil {
			h.reposAtUninstall = append(h.reposAtUninstall, len(st.Repos))
		}
		if h.onUninstall != nil {
			h.onUninstall()
		}
		if !h.loaded[req.ID] {
			return ipc.Response{OK: true, Status: ipc.StatusNotInstalled}
		}
		delete(h.loaded, req.ID)
		h.uninstalled = append(h.uninstalled, req.ID)
		return ipc.Response{OK: true, Status: ipc.StatusUninstalled}
	case ipc.CmdReload:
		if _, err := nmhost.ManagedIDs(req.IDs); err != nil {
			return ipc.Response{Error: err.Error()}
		}
		h.reloaded = append(h.reloaded, req.IDs...)
		results := make([]ipc.ReloadResult, len(req.IDs))
		for i, id := range req.IDs {
			results[i] = ipc.ReloadResult{ID: id, Status: ipc.StatusReloaded}
		}
		return ipc.Response{OK: true, Results: results}
	case ipc.CmdPing:
		return ipc.Response{OK: true, Host: &ipc.HostInfo{Version: "test", Leader: true}}
	}
	return ipc.Response{Error: "unknown command"}
}

// interactive makes the TTY-gated paths (prompts, Chrome-side removal) run,
// which is exactly where the command-order bugs live.
func interactive(t *testing.T) {
	t.Helper()
	orig := assist.IsTTY
	assist.IsTTY = func() bool { return true }
	t.Cleanup(func() { assist.IsTTY = orig })
}

// run executes a cepm command with the given stdin, returning its output.
func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	resetPromptReader()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func seedRepo(t *testing.T, name string, exts ...state.Extension) {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Repos[name] = &state.Repo{
		URL: "https://user:TOKEN@example.com/t/r.git", Track: state.TrackBranch,
		Branch: "main", Head: "abc", Extensions: exts,
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	dir, err := updaterRepoDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// updaterRepoDir avoids an import cycle in the test helper above.
func updaterRepoDir(name string) (string, error) {
	repos, err := paths.ReposDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(repos, name), nil
}

// uninstall used to unregister the repo before offering the Chrome-side
// removal, which made the host refuse every id as "not managed by cepm".
func TestUninstallRemovesFromChromeBeforeUnregistering(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "aaaa")
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa"})

	out, err := run(t, "y\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.uninstalled) != 1 || host.uninstalled[0] != "aaaa" {
		t.Errorf("Chrome-side removal did not happen: %v\n%s", host.uninstalled, out)
	}
	if strings.Contains(out, "not managed by cepm") {
		t.Errorf("removal was refused:\n%s", out)
	}
	// The ordering itself, not just the outcome: asking Chrome after the repo
	// is gone happens to work through the orphan record, so assert that the
	// request arrived while the repository was still registered.
	if len(host.reposAtUninstall) != 1 || host.reposAtUninstall[0] != 1 {
		t.Errorf("removal must be requested before unregistering, saw repo counts %v", host.reposAtUninstall)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := st.Repos["tools"]; still {
		t.Error("repo should be unregistered afterwards")
	}
}

// Declining leaves the extension in Chrome; the record has to survive the
// repo it belonged to, or cleanup can never finish the job.
func TestUninstallKeepsOrphanWhenUserDeclines(t *testing.T) {
	interactive(t)
	startFakeHost(t, "aaaa")
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa"})

	out, err := run(t, "n\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Orphans) != 1 || st.Orphans[0].ID != "aaaa" {
		t.Fatalf("expected an orphan record, got %+v", st.Orphans)
	}

	// cleanup can still remove it later.
	out, err = run(t, "y\n", "cleanup")
	if err != nil {
		t.Fatalf("cleanup failed: %v\n%s", err, out)
	}
	st, _ = state.Load()
	if len(st.Orphans) != 0 {
		t.Errorf("cleanup should clear the orphan, got %+v", st.Orphans)
	}
}

// While cleanup waits on Chrome's confirmation dialog it must hold the update
// lock: otherwise an automatic update can revive the extension between the
// liveness check and the user pressing "Remove", and the removal then hits a
// working extension. The fake host stands in for the dialog and probes the
// lock at exactly that moment.
func TestCleanupExcludesUpdatesWhileDialogIsOpen(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "aaaa")
	host.onUninstall = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := updater.WithLock(ctx, func() error { return nil }); err == nil {
			t.Error("the update lock must be held while a removal dialog is pending")
		}
	}
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa"})
	if out, err := run(t, "n\n", "uninstall", "tools"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if out, err := run(t, "", "cleanup"); err != nil {
		t.Fatalf("cleanup: %v\n%s", err, out)
	}
}

// One id, several records (a stale entry and an orphan): the summary counts
// what happened in Chrome, not how many records pointed at it.
func TestCleanupCountsUniqueIDs(t *testing.T) {
	interactive(t)
	startFakeHost(t, "aaaa")
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa"})
	if out, err := run(t, "n\n", "uninstall", "tools"); err != nil { // → orphan aaaa
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	// A second record for the same id, via a repo-level stale entry.
	st, _ := state.Load()
	seedRepo(t, "other", state.Extension{Dir: "x", Name: "X", ID: "bbbb"})
	st, _ = state.Load()
	st.Repos["other"].AddStale(state.StaleExtension{ID: "aaaa", Name: "Ext", Reason: "removed"})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "cleanup")
	if err != nil {
		t.Fatalf("cleanup: %v\n%s", err, out)
	}
	if strings.Contains(out, "left in Chrome") {
		t.Errorf("nothing failed, so no retry message should appear:\n%s", out)
	}
	if !strings.Contains(out, "Removed 1 extension(s)") {
		t.Errorf("one unique id was removed once:\n%s", out)
	}
}

// writeRawState puts a state.json on disk without going through Save, so a
// test can reproduce a file an older cepm was able to write.
func writeRawState(t *testing.T, content string) {
	t.Helper()
	p, err := paths.StateFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Versions before the uniqueness rule could save two extensions sharing an
// id. Acting on such a file would remove one repository's extension from
// Chrome on behalf of another, so commands that touch Chrome must stop first
// — and uninstall, the way out, must not touch Chrome either.
func TestDuplicateIDsInExistingStateStopChromeSideEffects(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":2,"repos":{
      "a":{"url":"u1","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"One","id":"xxxx","key":"K"}]},
      "b":{"url":"u2","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"other","name":"Two","id":"xxxx","key":"K"}]}}}`)

	for _, args := range [][]string{{"cleanup"}, {"reload"}, {"update"}, {"enable", "a"}} {
		if _, err := run(t, "", args...); err == nil {
			t.Errorf("%v should refuse to run on a state with duplicate ids", args)
		}
	}

	// uninstall is the repair path: it unregisters, but must not act on an
	// id it cannot attribute.
	out, err := run(t, "y\n", "uninstall", "b")
	if err != nil {
		t.Fatalf("uninstall should still work: %v\n%s", err, out)
	}
	host.mu.Lock()
	sent := len(host.uninstalled)
	host.mu.Unlock()
	if sent != 0 {
		t.Errorf("nothing should have been removed from Chrome, got %v", host.uninstalled)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Validate(); err != nil {
		t.Errorf("uninstalling one side should resolve the duplicate: %v", err)
	}
	// And now the normal commands work again.
	if _, err := run(t, "", "reload"); err != nil && strings.Contains(err.Error(), "claim extension id") {
		t.Errorf("commands should work once the duplicate is gone: %v", err)
	}
}

// Repairing has to work one repository at a time: with two independent
// collisions, a plain all-or-nothing save would reject every intermediate
// step and leave the file unfixable.
func TestUninstallRepairsSeveralDuplicateGroups(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "xxxx", "yyyy")
	writeRawState(t, `{"version":2,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"A","id":"xxxx","key":"K1"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"B","id":"xxxx","key":"K1"}]},
      "c":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"C","id":"yyyy","key":"K2"}]},
      "d":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"D","id":"yyyy","key":"K2"}]}}}`)

	for _, repo := range []string{"b", "d"} {
		if out, err := run(t, "y\n", "uninstall", repo); err != nil {
			t.Fatalf("uninstall %s: %v\n%s", repo, err, out)
		}
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Validate(); err != nil {
		t.Errorf("state should be repaired after both uninstalls: %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.uninstalled) != 0 {
		t.Errorf("repairing must not touch Chrome, got %v", host.uninstalled)
	}
}

// When two registrations claim an id, nothing can tell which directory
// Chrome loaded. Removing one side must therefore keep its files and point
// the user at the surviving directory — otherwise Chrome may be left running
// a copy from a path cepm just deleted, with doctor none the wiser.
func TestUninstallRepairGuidesRebindAndKeepsFiles(t *testing.T) {
	interactive(t)
	startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":2,"repos":{
      "keepme":{"url":"u","track":"branch","branch":"main","head":"h",
                "extensions":[{"dir":"ext","name":"Ext","id":"xxxx","key":"K"}]},
      "dropme":{"url":"u","track":"branch","branch":"main","head":"h",
                "extensions":[{"dir":"ext","name":"Ext","id":"xxxx","key":"K"}]}}}`)
	dropped, err := updaterRepoDir("dropme")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dropped, 0o755); err != nil {
		t.Fatal(err)
	}
	survivor, err := updaterRepoDir("keepme")
	if err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "uninstall", "dropme")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if !strings.Contains(out, filepath.Join(survivor, "ext")) {
		t.Errorf("the guidance must name the surviving directory to load:\n%s", out)
	}
	if _, err := os.Stat(dropped); err != nil {
		t.Errorf("the clone must be kept until Chrome is re-pointed: %v", err)
	}
	if strings.Contains(out, "run: cepm cleanup") {
		t.Errorf("cleanup cannot help here (the id is live again); do not suggest it:\n%s", out)
	}
}

// Mid-repair, with two claimants still left, there is no single directory to
// re-point Chrome at — guidance naming both would ask for two directories
// under one id.
func TestUninstallRepairSaysNothingToRebindWhileAmbiguous(t *testing.T) {
	interactive(t)
	startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":2,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"B","id":"xxxx","key":"K"}]},
      "c":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"C","id":"xxxx","key":"K"}]}}}`)

	out, err := run(t, "", "uninstall", "b")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if strings.Contains(out, "Load unpacked") {
		t.Errorf("two claimants remain, so no directory can be named yet:\n%s", out)
	}
	if !strings.Contains(out, "still claimed by more than one repository") {
		t.Errorf("the user should be told the collision is unresolved:\n%s", out)
	}
	if strings.Contains(out, "rm -rf") {
		t.Errorf("the clone must not be declared safe to delete yet:\n%s", out)
	}
}

// A survivor the user chose not to use must not be handed back to Chrome.
func TestUninstallRepairRespectsDisabledSurvivor(t *testing.T) {
	interactive(t)
	startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":2,"repos":{
      "keepme":{"url":"u","track":"branch","branch":"main","head":"h",
                "extensions":[{"dir":"ext","name":"Ext","id":"xxxx","key":"K","disabled":true}]},
      "dropme":{"url":"u","track":"branch","branch":"main","head":"h",
                "extensions":[{"dir":"ext","name":"Ext","id":"xxxx","key":"K"}]}}}`)

	out, err := run(t, "", "uninstall", "dropme")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if strings.Contains(out, "Load unpacked") {
		t.Errorf("the survivor is disabled; loading it would undo that choice:\n%s", out)
	}
	if !strings.Contains(out, "cepm enable") {
		t.Errorf("the way back should be mentioned:\n%s", out)
	}
}

// Legacy state can hold a directory name no current version would register.
// Every path cepm prints for it goes through quoting, which must not pass
// control characters through.
func TestControlCharactersInLegacyStateAreNeverPrintedRaw(t *testing.T) {
	// Nothing loaded in Chrome, so doctor prints the "Load unpacked <path>"
	// hint — a path built with the directory name and shell-quoted, which is
	// the route that bypassed escaping.
	startFakeHost(t)
	writeRawState(t, `{"version":2,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"h",
               "extensions":[{"dir":"ext\nFORGED[2K","name":"Ext","id":"aaaa"}]}}}`)

	docOut, _ := run(t, "", "doctor")
	_, enableErr := run(t, "", "enable", "tools")
	texts := map[string]string{"doctor output": docOut}
	if enableErr != nil {
		texts["enable error"] = enableErr.Error()
	}
	for label, s := range texts {
		if strings.Contains(s, "\x1b") {
			t.Errorf("%s contains a raw escape sequence:\n%q", label, s)
		}
		if strings.Contains(s, "ext\nFORGED") {
			t.Errorf("%s contains a raw newline from the directory name:\n%q", label, s)
		}
	}
	// And the command that would act on Chrome refuses outright.
	if _, err := run(t, "", "reload"); err == nil {
		t.Error("reload should refuse a state with such a directory name")
	}
}

// The error for a legacy directory name says to uninstall and re-install, so
// that has to actually work: repair progress cannot be measured in duplicate
// ids when there are none.
func TestUninstallRepairsControlCharacterOnlyState(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	writeRawState(t, `{"version":2,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"h",
               "extensions":[{"dir":"ext\nFORGED","name":"Ext","id":"aaaa"}]}}}`)

	out, err := run(t, "", "uninstall", "tools")
	if err != nil {
		t.Fatalf("the repair cepm suggests must be possible: %v\n%s", err, out)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Validate(); err != nil {
		t.Errorf("state should be usable again: %v", err)
	}
	if _, still := st.Repos["tools"]; still {
		t.Error("the repository should be gone")
	}
}

// An explicit --keep-files is a promise, and the repair path used not to see
// the flag at all.
func TestRepairUninstallHonoursKeepFiles(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	writeRawState(t, `{"version":3,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"h",
               "extensions":[{"dir":"ext\nFORGED","name":"Ext","id":"aaaa"}]}}}`)
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if out, err := run(t, "", "uninstall", "--keep-files", "tools"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("--keep-files was ignored: %v", err)
	}
}

// A repository can hold stale records for ids it already lost track of.
// Dropping them with the repository would put those Chrome entries beyond
// cleanup's reach.
func TestRepairUninstallCarriesStaleEntriesOver(t *testing.T) {
	interactive(t)
	startFakeHost(t, "xxxx", "sssss")
	writeRawState(t, `{"version":3,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"B","id":"xxxx","key":"K"}],
           "stale":[{"id":"sssss","name":"Gone","reason":"removed"}]}}}`)

	if out, err := run(t, "", "uninstall", "b"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range st.Orphans {
		if o.ID == "sssss" {
			found = true
		}
	}
	if !found {
		t.Errorf("the repository's stale entry should survive as an orphan: %+v", st.Orphans)
	}
}

// Aborting because the repository changed is only safe if nothing was done to
// Chrome first — otherwise "nothing was changed" is a lie.
func TestUninstallDoesNotTouchChromeWhenAborting(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "aaaa")
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa"})

	// An update lands while the user is being asked. It *adds* an extension
	// rather than replacing one, so the extension under discussion stays
	// registered: otherwise the host's own authorization would refuse the
	// removal and hide whether the ordering is right.
	askedOnce := false
	assist.IsTTY = func() bool {
		if !askedOnce {
			askedOnce = true
			if err := updater.WithLock(context.Background(), func() error {
				st, err := state.Load()
				if err != nil {
					return err
				}
				st.Repos["tools"].Extensions = append(st.Repos["tools"].Extensions,
					state.Extension{Dir: "added", Name: "Added", ID: "zzzz"})
				return st.Save()
			}); err != nil {
				t.Errorf("simulating a concurrent update: %v", err)
			}
		}
		return true
	}

	out, err := run(t, "y\n", "uninstall", "tools")
	if err == nil {
		t.Fatalf("uninstall should abort:\n%s", out)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.uninstalled) != 0 {
		t.Errorf("nothing may be removed from Chrome before the abort, got %v", host.uninstalled)
	}
	if !host.loaded["aaaa"] {
		t.Error("the extension should still be loaded in Chrome")
	}
}

// Two such repositories mean the first removal leaves the state still
// invalid, so it goes through the repair save — which has to recognise a
// directory name as a defect, not only a duplicated id.
func TestUninstallRepairsControlCharactersOneRepoAtATime(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	writeRawState(t, `{"version":2,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext\nA","name":"A","id":"aaaa"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext\nB","name":"B","id":"bbbb"}]}}}`)

	if out, err := run(t, "", "uninstall", "a"); err != nil {
		t.Fatalf("first repair must be savable although the state stays invalid: %v\n%s", err, out)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := st.Repos["a"]; still {
		t.Fatal("the first removal was not persisted")
	}
	if err := st.Validate(); err == nil {
		t.Error("the second repository still has an unprintable name")
	}
	if out, err := run(t, "", "uninstall", "b"); err != nil {
		t.Fatalf("second repair: %v\n%s", err, out)
	}
	st, _ = state.Load()
	if err := st.Validate(); err != nil {
		t.Errorf("state should be usable again: %v", err)
	}
}

// A repair takes several uninstalls, and the clones kept along the way must
// not be forgotten once the ids stop being contested.
func TestUninstallRepairReportsEveryKeptClone(t *testing.T) {
	interactive(t)
	startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":2,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"B","id":"xxxx","key":"K"}]},
      "c":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"C","id":"xxxx","key":"K"}]}}}`)
	for _, r := range []string{"a", "b", "c"} {
		dir, err := updaterRepoDir(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if out, err := run(t, "", "uninstall", "b"); err != nil {
		t.Fatalf("first repair: %v\n%s", err, out)
	}
	out, err := run(t, "", "uninstall", "c")
	if err != nil {
		t.Fatalf("second repair: %v\n%s", err, out)
	}
	// Both kept clones, not just the one from this run, have to be named.
	for _, r := range []string{"b", "c"} {
		dir, _ := updaterRepoDir(r)
		if !strings.Contains(out, dir) {
			t.Errorf("the clone kept for %q is not mentioned in the final guidance:\n%s", r, out)
		}
	}
	st, _ := state.Load()
	if len(st.KeptClones) != 0 {
		t.Errorf("kept clones should be handed over once reported, got %+v", st.KeptClones)
	}
}

// Two directories of one repository claiming the same id: removing that
// repository resolves it outright, so its clone is not kept and the entry
// becomes an ordinary orphan cleanup can handle.
func TestUninstallRepairsSameRepoDuplicate(t *testing.T) {
	interactive(t)
	startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":2,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"h",
               "extensions":[{"dir":"one","name":"One","id":"xxxx","key":"K"},
                             {"dir":"two","name":"Two","id":"xxxx","key":"K"}]}}}`)
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	st, _ := state.Load()
	if err := st.Validate(); err != nil {
		t.Errorf("state should be valid: %v", err)
	}
	if len(st.Orphans) != 1 {
		t.Errorf("the id is nobody's now, so cleanup should be able to remove it: %+v", st.Orphans)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("no other registration claims the id, so the clone should be deleted")
	}
}

// Names are display-only, so a legacy one carrying a newline is cleaned when
// the state is read rather than at each place that prints it.
func TestLegacyNamesAreNeutralisedOnLoad(t *testing.T) {
	startFakeHost(t)
	writeRawState(t, `{"version":2,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"h",
               "extensions":[{"dir":"ext","name":"OK\nFORGED","id":"aaaa"}],
               "stale":[{"id":"bbbb","name":"Stale\u001b[2K","reason":"removed"}]}},
      "orphans":[{"id":"cccc","name":"Orphan\nRow","reason":"uninstalled"}]}`)

	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if n := st.Repos["tools"].Extensions[0].Name; strings.ContainsAny(n, "\n\x1b") {
		t.Errorf("extension name still has control characters: %q", n)
	}
	if n := st.Repos["tools"].Stale[0].Name; strings.ContainsAny(n, "\n\x1b") {
		t.Errorf("stale name still has control characters: %q", n)
	}
	if n := st.Orphans[0].Name; strings.ContainsAny(n, "\n\x1b") {
		t.Errorf("orphan name still has control characters: %q", n)
	}
	out, _ := run(t, "", "list")
	if strings.Contains(out, "OK\nFORGED") || strings.Contains(out, "\x1b") {
		t.Errorf("list printed a forged row:\n%q", out)
	}
}

// Three repositories sharing one id: each removal leaves the id colliding,
// so progress has to be measured in claims, not in colliding ids.
func TestUninstallRepairsThreeWayCollision(t *testing.T) {
	interactive(t)
	startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":2,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"B","id":"xxxx","key":"K"}]},
      "c":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext","name":"C","id":"xxxx","key":"K"}]}}}`)

	if out, err := run(t, "", "uninstall", "b"); err != nil {
		t.Fatalf("first repair must be savable even though the id still collides: %v\n%s", err, out)
	}
	st, _ := state.Load()
	if err := st.Validate(); err == nil {
		t.Error("two claimants remain, so the state is still invalid")
	}
	if len(st.Repos) != 2 {
		t.Fatalf("the first removal was not persisted: %+v", st.RepoNames())
	}
	if out, err := run(t, "", "uninstall", "c"); err != nil {
		t.Fatalf("second repair: %v\n%s", err, out)
	}
	st, _ = state.Load()
	if err := st.Validate(); err != nil {
		t.Errorf("state should be repaired now: %v", err)
	}
}

// State written before directory names were validated can still hold control
// characters, and those refs reach the terminal through validation errors.
func TestControlCharactersInStateCannotForgeOutput(t *testing.T) {
	startFakeHost(t)
	writeRawState(t, `{"version":2,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"ext\nFORGED[2K","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"h",
           "extensions":[{"dir":"other","name":"B","id":"xxxx","key":"K"}]}}}`)

	// preflight (a Chrome-affecting command) and doctor both print the refs.
	_, err := run(t, "", "reload")
	if err == nil {
		t.Fatal("reload should refuse a duplicated state")
	}
	docOut, _ := run(t, "", "doctor")
	for label, s := range map[string]string{"reload error": err.Error(), "doctor output": docOut} {
		if strings.Contains(s, "\x1b") {
			t.Errorf("%s contains a raw escape sequence:\n%q", label, s)
		}
		if strings.Contains(s, "ext\nFORGED") {
			t.Errorf("%s contains a raw newline from the directory name:\n%q", label, s)
		}
	}
}

// Records predating normalization can name an id that is live again. Cleanup
// must drop them, or doctor asks forever for a cleanup that does nothing.
func TestCleanupDropsRecordsForLiveIDs(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "aaaa")
	writeRawState(t, `{"version":2,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"h",
               "extensions":[{"dir":"ext","name":"Ext","id":"aaaa"}],
               "stale":[{"id":"aaaa","name":"Ext","reason":"removed"}]}},
      "orphans":[{"id":"aaaa","name":"Ext","reason":"uninstalled"}]}`)

	out, err := run(t, "", "cleanup")
	if err != nil {
		t.Fatalf("cleanup: %v\n%s", err, out)
	}
	host.mu.Lock()
	sent := len(host.uninstalled)
	host.mu.Unlock()
	if sent != 0 {
		t.Errorf("a live extension must not be removed from Chrome, got %v", host.uninstalled)
	}
	st, _ := state.Load()
	if len(st.Orphans) != 0 || len(st.Repos["tools"].Stale) != 0 {
		t.Errorf("records for a live id should be gone: orphans=%+v stale=%+v", st.Orphans, st.Repos["tools"].Stale)
	}
	out, err = run(t, "", "cleanup")
	if err != nil {
		t.Fatalf("second cleanup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Nothing to clean up.") {
		t.Errorf("a second run should have nothing to do:\n%s", out)
	}
}

// The lock must be released between entries: holding it across a whole batch
// scales with the number of dialogs and starves the background updater.
func TestCleanupReleasesLockBetweenEntries(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "aaaa", "bbbb")
	seedRepo(t, "tools",
		state.Extension{Dir: "a", Name: "A", ID: "aaaa"},
		state.Extension{Dir: "b", Name: "B", ID: "bbbb"},
	)
	st, _ := state.Load()
	st.Repos["tools"].Extensions = nil
	st.Repos["tools"].AddStale(state.StaleExtension{ID: "aaaa", Name: "A", Reason: "removed"})
	st.Repos["tools"].AddStale(state.StaleExtension{ID: "bbbb", Name: "B", Reason: "removed"})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	// Observed at each dialog: is the lock held, and has the previous entry
	// already been committed? Committing per entry is what proves the lock is
	// taken and released once per id — with a single batch-wide lock the
	// state would only change after the last dialog.
	var priorCommitted []bool
	host.onUninstall = func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := updater.WithLock(ctx, func() error { return nil }); err == nil {
			t.Error("the lock must be held while a dialog is pending")
		}
		st, err := state.Load()
		if err != nil {
			t.Error(err)
			return
		}
		stillRecorded := false
		for _, s := range st.Repos["tools"].Stale {
			if s.ID == "aaaa" {
				stillRecorded = true
			}
		}
		priorCommitted = append(priorCommitted, !stillRecorded)
	}

	if out, err := run(t, "", "cleanup"); err != nil {
		t.Fatalf("cleanup: %v\n%s", err, out)
	}
	if len(priorCommitted) != 2 {
		t.Fatalf("expected two dialogs, saw %d", len(priorCommitted))
	}
	if priorCommitted[0] {
		t.Error("nothing should be committed before the first dialog")
	}
	if !priorCommitted[1] {
		t.Error("the first entry must be committed before the second dialog: the lock is per entry, not per batch")
	}
}

// Extension ids are derived deterministically, so reinstalling a repo brings
// back an id that an earlier uninstall recorded as an orphan. Cleanup must
// not then remove the extension that is working again.
func TestCleanupSpareseLiveExtensionThatWasOrphaned(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "aaaa")
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa"})

	if out, err := run(t, "n\n", "uninstall", "tools"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	// Reinstalling the same repo at the same path yields the same id.
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa"})

	out, err := run(t, "y\n", "cleanup")
	if err != nil {
		t.Fatalf("cleanup: %v\n%s", err, out)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.uninstalled) != 0 {
		t.Errorf("cleanup removed a live extension: %v\n%s", host.uninstalled, out)
	}
	st, _ := state.Load()
	if len(st.Orphans) != 0 {
		t.Errorf("the orphan record should be gone once the id is live again: %+v", st.Orphans)
	}
}

// The load ceremony really does reach for the browser and the clipboard —
// this is why the package stubs them in TestMain. Asserting the calls happen
// keeps that stubbing necessary and visible: without it, every run of the
// suite opens a tab in the developer's Chrome and overwrites what they copied.
func TestLoadCeremonyUsesTheStubbedSideEffects(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	opened, copied := 0, 0
	origOpen, origCopy := assist.OpenExtensionsPage, assist.CopyToClipboard
	assist.OpenExtensionsPage = func() error { opened++; return nil }
	assist.CopyToClipboard = func(string) error { copied++; return nil }
	t.Cleanup(func() { assist.OpenExtensionsPage, assist.CopyToClipboard = origOpen, origCopy })

	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa", Disabled: true})
	// The extension is not in Chrome, so the ceremony would poll; a short
	// context keeps the test quick.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"enable", "tools/ext"})
	resetPromptReader()
	_ = root.ExecuteContext(ctx)

	if opened == 0 || copied == 0 {
		t.Errorf("the ceremony should have tried to open Chrome and copy the path (opened=%d copied=%d)", opened, copied)
	}
}

func TestEnableDisableRoundTrip(t *testing.T) {
	interactive(t)
	// Both are already in Chrome, so the load ceremony confirms at once
	// instead of polling for its full timeout.
	startFakeHost(t, "aaaa", "bbbb")
	seedRepo(t, "tools",
		state.Extension{Dir: "a", Name: "A", ID: "aaaa"},
		state.Extension{Dir: "b", Name: "B", ID: "bbbb", Disabled: true},
	)

	if out, err := run(t, "", "enable", "tools/b"); err != nil {
		t.Fatalf("enable: %v\n%s", err, out)
	}
	st, _ := state.Load()
	if !st.Repos["tools"].FindExtension("b").Enabled() {
		t.Error("enable did not persist")
	}

	if out, err := run(t, "n\n", "disable", "tools/a"); err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	st, _ = state.Load()
	if st.Repos["tools"].FindExtension("a").Enabled() {
		t.Error("disable did not persist")
	}
}

// Repository URLs can carry a token; no command may print it.
func TestListDoesNotLeakCredentials(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: "aaaa"})

	for _, args := range [][]string{{"list"}, {"list", "--json"}} {
		out, err := run(t, "", args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if strings.Contains(out, "TOKEN") {
			t.Errorf("%v leaked the token:\n%s", args, out)
		}
	}
	out, _ := run(t, "", "list", "--json")
	var payload struct {
		Repos []struct {
			URL string `json:"url"`
		} `json:"repos"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("list --json is not valid JSON: %v\n%s", err, out)
	}
	if len(payload.Repos) != 1 || !strings.Contains(payload.Repos[0].URL, "***") {
		t.Errorf("url should be redacted, got %+v", payload.Repos)
	}
}

// Switching Chromes must move the registration, not copy it: a manifest left
// behind would let two Chromes connect, of which only one receives reloads.
func TestSetupRegistersExactlyOneChrome(t *testing.T) {
	if len(paths.ChromeVariants) < 2 {
		t.Skip("needs at least two Chrome variants")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CEPM_HOME", filepath.Join(home, "cepm"))
	v0, v1 := paths.ChromeVariants[0], paths.ChromeVariants[1]

	manifestPath := func(variant string) string {
		dir, err := paths.NativeMessagingHostsDir(variant)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, nmmanifest.FileName())
	}

	if out, err := run(t, "", "setup", "--chrome-variant", v0); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if _, err := os.Stat(manifestPath(v0)); err != nil {
		t.Fatalf("manifest missing for %s: %v", v0, err)
	}

	if out, err := run(t, "", "setup", "--chrome-variant", v1); err != nil {
		t.Fatalf("setup switch: %v\n%s", err, out)
	}
	if _, err := os.Stat(manifestPath(v1)); err != nil {
		t.Fatalf("manifest missing for %s: %v", v1, err)
	}
	if _, err := os.Stat(manifestPath(v0)); !os.IsNotExist(err) {
		t.Errorf("manifest for %s should be removed after switching", v0)
	}
}

// Every install failure path prints something; none of them may print the
// URL as given, because it can carry a token.
func TestInstallErrorsDoNotLeakCredentials(t *testing.T) {
	startFakeHost(t)
	const url = "https://user:TOKEN@example.com/team/repo.git"

	dir, err := updaterRepoDir("repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "", "install", url)
	if err == nil {
		t.Fatal("install should fail here")
	}
	if combined := out + err.Error(); strings.Contains(combined, "TOKEN") {
		t.Errorf("install leaked the token:\n%s", combined)
	}
}

// The id-collision path is the newest install error and the one that echoed
// its argument. Drive it for real: a repository whose extension pins a key
// another registration already owns.
func TestInstallCollisionErrorDoesNotEchoTheURL(t *testing.T) {
	startFakeHost(t)
	const key = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0lLejiTvG5ElQmwA+FNOPTFTArbjNA65OVcj5zk3efV/myX/PK/TWO7oGT1BE/9zZfbozbaAMwrk6l8FoRVMGqmPaPCfdDdbtJ+ogS+6Evw9EJ3Tx+2oLUS+ddyzLbsMkoeXe0wvDIX4vOnwi1tULgTpxBlsSQ2zF5e8oZG+wMZRb3s8iPDwskfxrqFSgAaDuNH1vmZiRzOqnz+uLNwdjGHpMrP4KTeGbrAW71EBhYFT0eT47ScdgYodPS1LnfnIobpC5ALPIsIcJnDPKNfL//rlfi4/pGXRq08jOSb1z9nz4sMNTfiHl7shswdTSM1aUu9rsIF1fWmJPXVdQ2IbZQIDAQAB"
	keyID, err := extid.ForExtension("/unused-with-key", key)
	if err != nil {
		t.Fatal(err)
	}
	seedRepo(t, "existing", state.Extension{Dir: "ext", Name: "Ext", ID: keyID, Key: key})

	// A local repository providing an extension with the same key.
	src := t.TempDir()
	origin := filepath.Join(src, "origin.git")
	work := filepath.Join(src, "work")
	gitCmd(t, src, "init", "-q", "--bare", "--initial-branch=main", origin)
	gitCmd(t, src, "clone", "-q", origin, work)
	gitCmd(t, work, "checkout", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(work, "ext"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "ext", "manifest.json"),
		[]byte(`{"manifest_version":3,"name":"Same","version":"1.0","key":"`+key+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", "-A")
	gitCmd(t, work, "commit", "-qm", "init")
	gitCmd(t, work, "push", "-q", "origin", "main")

	out, err := run(t, "", "install", origin, "--name", "newone")
	if err == nil {
		t.Fatal("install should refuse the colliding extension")
	}
	combined := out + err.Error()
	if !strings.Contains(combined, "newone") {
		t.Errorf("the error should name the repository:\n%s", combined)
	}
	if strings.Contains(err.Error(), origin) {
		t.Errorf("the error must not echo the URL (it can carry a token):\n%s", err.Error())
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// doctor is documented as the way to verify an install, so it must fail loudly.
func TestDoctorExitsNonZeroOnFailure(t *testing.T) {
	startFakeHost(t)
	if _, err := run(t, "", "doctor"); err == nil {
		t.Error("doctor should report failure when setup has not run")
	}
}
