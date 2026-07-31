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

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
)

// fakeHost stands in for the native messaging host: it serves the control
// socket the CLI talks to, so command-level tests exercise the real IPC path
// (which is where a broken call order shows up).
type fakeHost struct {
	mu          sync.Mutex
	loaded      map[string]bool // extensions Chrome currently has
	uninstalled []string        // ids the CLI asked to remove
	reloaded    []string
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
		if !h.loaded[req.ID] {
			return ipc.Response{OK: true, Status: ipc.StatusNotInstalled}
		}
		delete(h.loaded, req.ID)
		h.uninstalled = append(h.uninstalled, req.ID)
		return ipc.Response{OK: true, Status: ipc.StatusUninstalled}
	case ipc.CmdReload:
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

// doctor is documented as the way to verify an install, so it must fail loudly.
func TestDoctorExitsNonZeroOnFailure(t *testing.T) {
	startFakeHost(t)
	if _, err := run(t, "", "doctor"); err == nil {
		t.Error("doctor should report failure when setup has not run")
	}
}
