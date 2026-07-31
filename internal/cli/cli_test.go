package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/assist"
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

// doctor is documented as the way to verify an install, so it must fail loudly.
func TestDoctorExitsNonZeroOnFailure(t *testing.T) {
	startFakeHost(t)
	if _, err := run(t, "", "doctor"); err == nil {
		t.Error("doctor should report failure when setup has not run")
	}
}
