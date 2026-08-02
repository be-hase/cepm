package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// Fixture ids pinned by manifest keys. Validate re-derives every live id
// from its recorded key or path, so a made-up literal like "aaaa" no longer
// passes; a key-derived id does not depend on the temp directory, which is
// what lets tests share these as constants.
var (
	keyA, idA = fixtureKey("seed-a")
	keyB, idB = fixtureKey("seed-b")
	keyZ, idZ = fixtureKey("seed-z")
)

func fixtureKey(seed string) (key, id string) {
	return base64.StdEncoding.EncodeToString([]byte(seed)), extid.FromPublicKey([]byte(seed))
}

// fakeHost stands in for the native messaging host: it serves the control
// socket the CLI talks to, so command-level tests exercise the real IPC path
// (which is where a broken call order shows up).
type fakeHost struct {
	mu               sync.Mutex
	loaded           map[string]bool // extensions Chrome currently has
	disabledInChrome map[string]bool // loaded but switched off in chrome://extensions
	uninstalled      []string        // ids the CLI asked to remove
	reloaded         []string
	// reposAtUninstall records how many repositories were still registered
	// when a removal request arrived, which is what pins the ordering.
	reposAtUninstall []int
	// onUninstall runs while a removal request is being handled — the moment
	// the real Chrome would be showing its confirmation dialog.
	onUninstall func()
	// onList runs while a list request is being handled — for disable, that
	// is while its "remove from Chrome too?" prompt is being prepared, i.e.
	// the window in which another cepm can change the state.
	onList func()
	// Reload failure modes, for exit-code tests: a host-level error, per-id
	// error results, an empty result set (every item unanswered), or
	// not_installed answers.
	reloadHostError    string
	failReloads        bool
	dropReloadResults  bool
	reloadNotInstalled bool
	reloadStatusByID   map[string]string // per-id status override
	// Helper-compat presentation for doctor tests.
	helperVersion string
	helperTooOld  bool
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
		if h.onList != nil {
			h.onList()
		}
		var exts []ipc.ChromeExt
		for id := range h.loaded {
			exts = append(exts, ipc.ChromeExt{ID: id, Name: "Ext " + id, Version: "1.0",
				Enabled: !h.disabledInChrome[id]})
		}
		return ipc.Response{OK: true, Extensions: exts}
	case ipc.CmdUninstall:
		// Enforce the real authorization rule, not a stub: without this the
		// test would pass even if a command asked to remove an extension it
		// had already unregistered — the exact bug these tests exist for.
		if _, err := nmhost.AuthorizeRemoval([]string{req.ID}); err != nil {
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
		if _, err := nmhost.AuthorizeReload(req.IDs); err != nil {
			return ipc.Response{Error: err.Error()}
		}
		if h.reloadHostError != "" {
			return ipc.Response{Error: h.reloadHostError}
		}
		if h.dropReloadResults {
			return ipc.Response{OK: true}
		}
		h.reloaded = append(h.reloaded, req.IDs...)
		results := make([]ipc.ReloadResult, len(req.IDs))
		for i, id := range req.IDs {
			status := ipc.StatusReloaded
			switch {
			case h.reloadStatusByID[id] != "":
				status = h.reloadStatusByID[id]
			case h.failReloads:
				status = ipc.StatusError
			case h.reloadNotInstalled:
				status = ipc.StatusNotInstalled
			}
			results[i] = ipc.ReloadResult{ID: id, Status: status}
		}
		return ipc.Response{OK: true, Results: results}
	case ipc.CmdPing:
		return ipc.Response{OK: true, Host: &ipc.HostInfo{
			Version: "test", Leader: true,
			HelperVersion: h.helperVersion, MinHelperVersion: "0.1.0",
			HelperOK: !h.helperTooOld,
		}}
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
		Branch: "main", Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Extensions: exts,
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
	host := startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	out, err := run(t, "y\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.uninstalled) != 1 || host.uninstalled[0] != idA {
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
	startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	out, err := run(t, "n\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Orphans) != 1 || st.Orphans[0].ID != idA {
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
	host := startFakeHost(t, idA)
	host.onUninstall = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := updater.WithLock(ctx, func() error { return nil }); err == nil {
			t.Error("the update lock must be held while a removal dialog is pending")
		}
	}
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
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
	startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	if out, err := run(t, "n\n", "uninstall", "tools"); err != nil { // → orphan aaaa
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	// A second record for the same id, via a repo-level stale entry.
	seedRepo(t, "other", state.Extension{Dir: "x", Name: "X", ID: idB, Key: keyB})
	st, _ := state.Load()
	st.Repos["other"].AddStale(state.StaleExtension{ID: idA, Name: "Ext", Reason: "removed"})
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
	writeRawState(t, `{"version":6,"repos":{
      "a":{"url":"u1","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext","name":"One","id":"xxxx","key":"K"}]},
      "b":{"url":"u2","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"other","name":"Two","id":"xxxx","key":"K"}]}}}`)

	for _, args := range [][]string{{"cleanup"}, {"reload"}, {"update"}, {"enable", "a"}, {"uninstall", "b"}} {
		if _, err := run(t, "", args...); err == nil {
			t.Errorf("%v should refuse to run on a state with duplicate ids", args)
		}
	}
	host.mu.Lock()
	sent := len(host.uninstalled)
	host.mu.Unlock()
	if sent != 0 {
		t.Errorf("nothing should have been removed from Chrome, got %v", host.uninstalled)
	}
}

// Legacy state can hold a directory name no current version would register.
// Every path cepm prints for it goes through quoting, which must not pass
// control characters through.
// An invalid state (a duplicate id, an unprintable directory) is not
// something any current cepm can save, so on disk it means corruption or a
// hand edit. There is no repair mode: every state-changing command —
// uninstall included — must refuse it before touching Chrome or the clones,
// and say how to start over.
func TestUninstallFailsClosedOnInvalidState(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":6,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext","name":"B","id":"xxxx","key":"K"}]}}}`)
	dirA, err := updaterRepoDir("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "y\n", "uninstall", "a")
	if err == nil {
		t.Fatalf("uninstall must refuse an invalid state:\n%s", out)
	}
	if !strings.Contains(err.Error(), "cepm reset") {
		t.Errorf("the error should say how to start over (cepm reset), got: %v", err)
	}
	host.mu.Lock()
	if len(host.uninstalled) != 0 {
		t.Errorf("nothing may be removed from Chrome, got %v", host.uninstalled)
	}
	host.mu.Unlock()
	if _, err := os.Stat(dirA); err != nil {
		t.Errorf("the clone must be left alone: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Repos) != 2 {
		t.Errorf("the state must be left alone, got %d repos", len(st.Repos))
	}
}

// The recovery that the fail-closed error points at has to actually work.
// Deleting state.json alone would strand the clones: install refuses the
// occupied directory, and uninstall no longer knows the name. reset moves
// state and clones aside together, and deletes nothing.
func TestResetMovesStateAndClonesToABackup(t *testing.T) {
	host := startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":6,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext","name":"B","id":"xxxx","key":"K"}]}}}`)
	dirA, err := updaterRepoDir("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "keep.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "reset")
	if err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}

	// The old world is out of the way…
	home := os.Getenv("CEPM_HOME")
	if _, err := os.Stat(filepath.Join(home, "state.json")); err == nil {
		t.Error("state.json should have been moved away")
	}
	repos, _ := paths.ReposDir()
	if _, err := os.Stat(repos); err == nil {
		t.Error("the repos directory should have been moved away")
	}
	// …but nothing was deleted: both live on in the backup.
	backups, err := filepath.Glob(filepath.Join(home, "backup-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one backup directory, got %v (%v)", backups, err)
	}
	if _, err := os.Stat(filepath.Join(backups[0], "state.json")); err != nil {
		t.Errorf("the backup should hold the old state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backups[0], "repos", "a", "keep.txt")); err != nil {
		t.Errorf("the backup should hold the clones intact: %v", err)
	}
	// Chrome was not touched, and cepm starts from a clean slate.
	host.mu.Lock()
	if len(host.uninstalled) != 0 {
		t.Errorf("reset must not touch Chrome, got %v", host.uninstalled)
	}
	host.mu.Unlock()
	if _, err := run(t, "", "list"); err != nil {
		t.Errorf("cepm should be usable again after reset: %v", err)
	}
}

// Reset is a state writer like any other, so it has to hold the update lock:
// racing an in-flight update would move the clones out from under it and let
// its final save recreate a state pointing into the backup.
func TestResetWaitsForTheUpdateLock(t *testing.T) {
	startFakeHost(t)

	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- updater.WithLock(context.Background(), func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	resetDone := make(chan struct{})
	go func() {
		_, _ = run(t, "", "reset")
		close(resetDone)
	}()
	select {
	case <-resetDone:
		t.Fatal("reset must wait for the update lock, but it finished while the lock was held")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-resetDone:
	case <-time.After(10 * time.Second):
		t.Fatal("reset did not finish after the lock was released")
	}
}

// If the second move fails, the first must be undone: metadata in the backup
// with the clones still active is exactly the split state reset exists to
// resolve.
func TestResetRollsBackWhenACloneCannotMove(t *testing.T) {
	startFakeHost(t)
	writeRawState(t, `{"version":6,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext","name":"A","id":"aaaa"}]}}}`)
	dirA, err := updaterRepoDir("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}

	orig := renameForBackup
	renameForBackup = func(src, dst string) error {
		if filepath.Base(src) == "repos" {
			return fmt.Errorf("injected rename failure")
		}
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { renameForBackup = orig })

	out, err := run(t, "", "reset")
	if err == nil {
		t.Fatalf("reset should report the failed move:\n%s", out)
	}
	home := os.Getenv("CEPM_HOME")
	if _, err := os.Stat(filepath.Join(home, "state.json")); err != nil {
		t.Errorf("state.json must be rolled back to its place: %v", err)
	}
	if _, err := os.Stat(dirA); err != nil {
		t.Errorf("the clone must still be active: %v", err)
	}
	if backups, _ := filepath.Glob(filepath.Join(home, "backup-*")); len(backups) != 0 {
		t.Errorf("a failed reset should not leave a backup behind: %v", backups)
	}
}

// The recovery guidance must match what the backup actually holds: a missing
// or unparseable state.json cannot be the place to look up the repo URLs —
// but every moved clone still knows its own origin.
func TestResetGuidesRecoveryWithoutAReadableState(t *testing.T) {
	t.Run("no state file", func(t *testing.T) {
		startFakeHost(t)
		dirA, err := updaterRepoDir("a")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dirA, 0o755); err != nil {
			t.Fatal(err)
		}

		out, err := run(t, "", "reset")
		if err != nil {
			t.Fatalf("reset: %v\n%s", err, out)
		}
		if !strings.Contains(out, "remote get-url origin") {
			t.Errorf("without a state file the clones are the URL source:\n%s", out)
		}
		if strings.Contains(out, "repository URLs are in") {
			t.Errorf("must not point at a state.json that does not exist in the backup:\n%s", out)
		}
	})

	// Parseable is not the same as useful: these two load-rejected states
	// unmarshal fine yet hold no URL, so the clones are still the source.
	for label, raw := range map[string]string{
		"null repo entry": `{"version":6,"repos":{"a":null}}`,
		"missing url":     `{"version":6,"repos":{"a":{"track":"branch","branch":"main"}}}`,
	} {
		t.Run(label, func(t *testing.T) {
			startFakeHost(t)
			writeRawState(t, raw)
			dirA, err := updaterRepoDir("a")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dirA, 0o755); err != nil {
				t.Fatal(err)
			}

			out, err := run(t, "", "reset")
			if err != nil {
				t.Fatalf("reset: %v\n%s", err, out)
			}
			if !strings.Contains(out, "remote get-url origin") {
				t.Errorf("a state without URLs cannot be the URL source — point at the clones:\n%s", out)
			}
			if strings.Contains(out, "repository URLs are in") {
				t.Errorf("must not claim the backed-up state holds the URLs:\n%s", out)
			}
		})
	}

	t.Run("corrupt state file", func(t *testing.T) {
		startFakeHost(t)
		writeRawState(t, `{definitely not json`)
		dirA, err := updaterRepoDir("a")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dirA, 0o755); err != nil {
			t.Fatal(err)
		}

		out, err := run(t, "", "reset")
		if err != nil {
			t.Fatalf("reset must work exactly when the state is broken: %v\n%s", err, out)
		}
		if !strings.Contains(out, "remote get-url origin") {
			t.Errorf("an unparseable state cannot be the URL source:\n%s", out)
		}
		home := os.Getenv("CEPM_HOME")
		backups, _ := filepath.Glob(filepath.Join(home, "backup-*"))
		if len(backups) != 1 {
			t.Fatalf("expected one backup, got %v", backups)
		}
		if _, err := os.Stat(filepath.Join(backups[0], "state.json")); err != nil {
			t.Errorf("even a broken state file belongs in the backup: %v", err)
		}
	})
}

// The control socket's authorization is the native host's preflight: Chrome
// starts the host without going through the CLI, so an invalid state has to
// fail closed here too or reload/uninstall would still be relayed.
func TestAuthorizationRefusesInvalidState(t *testing.T) {
	startFakeHost(t, "xxxx")
	writeRawState(t, `{"version":6,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext","name":"B","id":"xxxx","key":"K"}]}}}`)
	if _, err := nmhost.AuthorizeReload([]string{"xxxx"}); err == nil {
		t.Error("an invalid state must not authorize any reload")
	}
	if _, err := nmhost.AuthorizeRemoval([]string{"xxxx"}); err == nil {
		t.Error("an invalid state must not authorize any removal")
	}
}

func TestControlCharactersInLegacyStateAreNeverPrintedRaw(t *testing.T) {
	// Nothing loaded in Chrome, so doctor prints the "Load unpacked <path>"
	// hint — a path built with the directory name and shell-quoted, which is
	// the route that bypassed escaping.
	startFakeHost(t)
	writeRawState(t, `{"version":6,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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

// failStateSaves makes writing state.json fail while everything else (the
// lock, loading, deleting clones) keeps working. It has to be an injected
// fault: EnsureLayout repairs directory permissions on every lock
// acquisition, so a read-only CEPM_HOME does not survive into the save.
func failStateSaves(t *testing.T) {
	t.Helper()
	t.Cleanup(state.FailSaves())
}

// A failed save must not take the clone with it: the repository would stay
// registered with nothing behind it, and re-running could not repair that.
func TestUninstallKeepsTheCloneWhenTheSaveFails(t *testing.T) {
	interactive(t)
	startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	failStateSaves(t)

	// "n" to the Chrome question: whether Chrome untouched-on-failure holds
	// is TestUninstallDoesNotTouchChromeWhenAborting's business.
	out, err := run(t, "n\n", "uninstall", "tools")
	if err == nil {
		t.Fatalf("uninstall should fail when the state cannot be saved:\n%s", out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the clone must survive a failed save: %v", err)
	}
}

// Aborting because the repository changed is only safe if nothing was done to
// Chrome first — otherwise "nothing was changed" is a lie.
func TestUninstallDoesNotTouchChromeWhenAborting(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

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
					state.Extension{Dir: "added", Name: "Added", ID: idZ, Key: keyZ})
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
	if !host.loaded[idA] {
		t.Error("the extension should still be loaded in Chrome")
	}
}

// "Absent from Chrome" judged at the prompt must not be trusted at removal
// time: an extension loaded meanwhile (cepm enable in another terminal)
// would otherwise lose both its files and its orphan record — Chrome keeps
// running it, and cleanup can never find it again.
func TestUninstallOrphansAnExtensionLoadedDuringThePrompt(t *testing.T) {
	interactive(t)
	// idA is registered but not loaded; idB is loaded, so it triggers
	// the prompt that gives the concurrent load a window.
	host := startFakeHost(t, idB)
	seedRepo(t, "tools",
		state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA},
		state.Extension{Dir: "other", Name: "Other", ID: idB, Key: keyB})

	loadedOnce := false
	assist.IsTTY = func() bool {
		if !loadedOnce {
			loadedOnce = true
			host.mu.Lock()
			host.loaded[idA] = true
			host.mu.Unlock()
		}
		return true
	}

	// "n": keep bbbb in Chrome, so both ids must end up as orphans.
	out, err := run(t, "n\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, o := range st.Orphans {
		ids[o.ID] = true
	}
	if !ids[idA] {
		t.Errorf("the extension loaded during the prompt must get an orphan record, got %+v", st.Orphans)
	}
	if !ids[idB] {
		t.Errorf("the declined extension must keep its orphan record, got %+v", st.Orphans)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if !host.loaded[idA] {
		t.Error("nothing was approved, so Chrome must still have the extension")
	}
}

// Names are display-only, so a legacy one carrying a newline is cleaned when
// the state is read rather than at each place that prints it.
func TestLegacyNamesAreNeutralisedOnLoad(t *testing.T) {
	startFakeHost(t)
	writeRawState(t, `{"version":6,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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

// State written before directory names were validated can still hold control
// characters, and those refs reach the terminal through validation errors.
func TestControlCharactersInStateCannotForgeOutput(t *testing.T) {
	startFakeHost(t)
	writeRawState(t, `{"version":6,"repos":{
      "a":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
           "extensions":[{"dir":"ext\nFORGED\u001b[2K","name":"A","id":"xxxx","key":"K"}]},
      "b":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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
	host := startFakeHost(t, idA)
	// Raw on purpose: Save normalizes these records away, so only a file
	// written by something else can hold them.
	writeRawState(t, fmt.Sprintf(`{"version":6,"repos":{
      "tools":{"url":"u","track":"branch","branch":"main","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
               "extensions":[{"dir":"ext","name":"Ext","id":%q,"key":%q}],
               "stale":[{"id":%q,"name":"Ext","reason":"removed"}]}},
      "orphans":[{"id":%q,"name":"Ext","reason":"uninstalled"}]}`,
		idA, keyA, idA, idA))

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
	host := startFakeHost(t, idA, idB)
	seedRepo(t, "tools",
		state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA},
		state.Extension{Dir: "b", Name: "B", ID: idB, Key: keyB},
	)
	st, _ := state.Load()
	st.Repos["tools"].Extensions = nil
	st.Repos["tools"].AddStale(state.StaleExtension{ID: idA, Name: "A", Reason: "removed"})
	st.Repos["tools"].AddStale(state.StaleExtension{ID: idB, Name: "B", Reason: "removed"})
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
			if s.ID == idA {
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
	host := startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	if out, err := run(t, "n\n", "uninstall", "tools"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	// Reinstalling the same repo at the same path yields the same id.
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

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

	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA, Disabled: true})
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
	startFakeHost(t, idA, idB)
	seedRepo(t, "tools",
		state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA},
		state.Extension{Dir: "b", Name: "B", ID: idB, Key: keyB, Disabled: true},
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

// Repository URLs can carry a token — in the userinfo, the query, or the
// fragment; no command may print any of them.
func TestListDoesNotLeakCredentials(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Repos["tools"].URL = "https://user:TOKEN@example.com/t/r.git?private_token=QSECRET#token=FSECRET"
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"list"}, {"list", "--json"}} {
		out, err := run(t, "", args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		for _, secret := range []string{"TOKEN", "QSECRET", "FSECRET"} {
			if strings.Contains(out, secret) {
				t.Errorf("%v leaked %s:\n%s", args, secret, out)
			}
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
	const url = "https://user:TOKEN@example.com/team/repo.git?access_token=QSECRET"

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
	combined := out + err.Error()
	for _, secret := range []string{"TOKEN", "QSECRET"} {
		if strings.Contains(combined, secret) {
			t.Errorf("install leaked %s:\n%s", secret, combined)
		}
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

// Install stages its clone outside repos/ and only renames it into place
// inside the update lock, together with the state entry. A reset that runs
// while install waits on the selection prompt must therefore never separate
// the two: whatever the interleaving, the registered repository and its
// clone end up on the same side.
func TestInstallSurvivesAConcurrentReset(t *testing.T) {
	interactive(t)
	host := startFakeHost(t)
	home := os.Getenv("CEPM_HOME")
	// Pretend Chrome already loaded the extension install will register, so
	// the post-install ceremony's wait confirms immediately instead of
	// polling for 90 seconds. Path-derived, so computable up front.
	dirTools, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	idOne, err := extid.FromPath(filepath.Join(dirTools, "one"))
	if err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.loaded[idOne] = true
	host.mu.Unlock()

	// Two extensions, so install stops at the selection prompt — the window
	// a concurrent reset can slip into.
	origin := filepath.Join(home, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, home, "init", "-q", "-b", "main", origin)
	for _, e := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(origin, e), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := `{"manifest_version":3,"name":"` + e + `","version":"1.0"}`
		if err := os.WriteFile(filepath.Join(origin, e, "manifest.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, origin, "add", "-A")
	gitCmd(t, origin, "commit", "-qm", "init")

	resetRan := false
	assist.IsTTY = func() bool {
		if !resetRan {
			resetRan = true
			if out, err := run(t, "", "reset"); err != nil {
				t.Errorf("concurrent reset: %v\n%s", err, out)
			}
		}
		return true
	}

	out, err := run(t, "1\n", "install", origin, "--name", "tools")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !resetRan {
		t.Fatal("the test never reached the selection prompt")
	}

	// State and clone must be on the same side: registered and present.
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Repos["tools"]; !ok {
		t.Fatal("the repository should be registered")
	}
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "one", "manifest.json")); err != nil {
		t.Errorf("the registered repository must have its clone: %v", err)
	}
	if err := st.Validate(); err != nil {
		t.Errorf("the installed state must be valid: %v", err)
	}
}

// An unparsable URL (a stray %-escape) cannot be split into safe and secret
// parts; every display path must drop it entirely rather than echo the
// token it may carry.
func TestInstallErrorsDoNotLeakCredentialsFromUnparsableURLs(t *testing.T) {
	startFakeHost(t)
	out, err := run(t, "", "install", "https://user:TOKEN@example.com/%ZZ/repo.git?access_token=QSECRET")
	if err == nil {
		t.Fatal("install should fail here")
	}
	combined := out + err.Error()
	for _, secret := range []string{"TOKEN", "QSECRET"} {
		if strings.Contains(combined, secret) {
			t.Errorf("install leaked %s:\n%s", secret, combined)
		}
	}
}
