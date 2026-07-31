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
