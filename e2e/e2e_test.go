//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/nmmanifest"
)

// TestE2E drives a real Chrome (Chrome for Testing, throwaway profile,
// --load-extension standing in for the one manual "Load unpacked") against a
// built cepm binary and verifies the full loop: helper connectivity, that
// "cepm update" really makes Chrome run the new code, updates while Chrome
// is closed, the periodic auto-update, and the helper self-refresh.
func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e is not run with -short")
	}
	chromeBin := ensureChrome(t) // before HOME is overridden

	// A killed run (a timeout, a Ctrl-C) never reaches its cleanup, and each
	// leftover holds a Chrome profile worth ~15MB. Sweep the ones this suite
	// can prove it abandoned before adding another.
	sweepStaleWorkDirs(t)

	// Read before HOME is replaced, so the build below reuses the real
	// caches. Left to default they would land in the fake HOME, and the
	// module cache is written read-only: the cleanup could not remove it.
	goEnv := realGoEnv(t)

	// Isolated world: a fake HOME so neither the user's Chrome nor their
	// ~/.cepm is touched, under /tmp to keep the Unix socket path short.
	home := newWorkDir(t)
	t.Cleanup(func() { removeWorkDir(t, home) })
	cepmHome := filepath.Join(home, "cepm")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CEPM_HOME", cepmHome)

	// Registered after the RemoveAll cleanup so it runs before it (LIFO).
	// The copy under /tmp survives the test for post-mortem debugging.
	t.Cleanup(func() {
		b, err := os.ReadFile(filepath.Join(cepmHome, "logs", "host.log"))
		if err != nil {
			return
		}
		_ = os.WriteFile("/tmp/cepm-e2e-host.log", b, 0o644)
		if t.Failed() {
			t.Logf("host.log (also at /tmp/cepm-e2e-host.log):\n%s", b)
		}
	})

	bin := buildCepm(t, home, goEnv)

	// A git "origin" with one extension, plus an author clone to push from.
	origin := filepath.Join(home, "origin.git")
	author := filepath.Join(home, "author")
	git(t, home, "init", "--bare", "--initial-branch=main", origin)
	git(t, home, "clone", origin, author)
	git(t, author, "checkout", "-b", "main")
	writeSWExtension(t, filepath.Join(author, "widget"), "1.0.0", "BUILD-1")
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "v1.0.0")
	git(t, author, "push", "origin", "main")

	runCepm(t, bin, "install", origin, "--name", "testrepo")
	runCepm(t, bin, "setup")
	// Shortest interval the config allows, so the periodic update scenario
	// finishes in test time (the first run is shortened further via
	// CEPM_BOOTSTRAP_UPDATE_DELAY when Chrome is launched).
	if err := os.WriteFile(filepath.Join(cepmHome, "config.toml"),
		[]byte("[update]\ninterval = \"1m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	extDir := filepath.Join(cepmHome, "repos", "testrepo", "widget")
	extID, err := extid.FromPath(extDir)
	if err != nil {
		t.Fatal(err)
	}
	helperDir := filepath.Join(cepmHome, "helper")
	profile := filepath.Join(home, "profile")
	placeNMManifest(t, home, profile)

	chrome, cdp := launchChrome(t, chromeBin, profile, helperDir, extDir)

	t.Log("scenario 1: helper connects and the extension runs BUILD-1")
	waitPing(t, 90*time.Second)
	if got := waitVersion(t, extID, "1.0.0", 30*time.Second); got != "1.0.0" {
		t.Fatalf("extension not loaded with v1.0.0 (got %q)", got)
	}
	waitBuild(t, cdp, extID, "BUILD-1", 30*time.Second)

	t.Log("scenario 2: cepm update makes Chrome run the new code")
	writeSWExtension(t, filepath.Join(author, "widget"), "1.0.0", "BUILD-2")
	git(t, author, "commit", "-am", "code update")
	git(t, author, "push", "origin", "main")
	out := runCepm(t, bin, "update")
	if !strings.Contains(out, "reloaded") {
		t.Errorf("update output should confirm the reload:\n%s", out)
	}
	waitBuild(t, cdp, extID, "BUILD-2", 30*time.Second)

	t.Log("scenario 3: a manifest.json change is reported as needing a Chrome restart")
	writeSWExtension(t, filepath.Join(author, "widget"), "1.1.0", "BUILD-3")
	git(t, author, "commit", "-am", "v1.1.0")
	git(t, author, "push", "origin", "main")
	out = runCepm(t, bin, "update")
	if !strings.Contains(out, "manifest.json changed") {
		t.Errorf("update must warn that manifest changes need a restart:\n%s", out)
	}
	waitBuild(t, cdp, extID, "BUILD-3", 30*time.Second) // code still applies
	if got := currentVersion(t, extID); got != "1.0.0" {
		t.Logf("note: Chrome reports version %q after a manifest change (expected the cached 1.0.0)", got)
	}
	if out := runCepm(t, bin, "doctor"); !strings.Contains(out, "manifest.json on disk is 1.1.0") {
		t.Errorf("doctor should flag the pending manifest change:\n%s", out)
	}

	t.Log("scenario 4: update while Chrome is closed applies on next launch")
	stopChrome(t, chrome)
	writeSWExtension(t, filepath.Join(author, "widget"), "1.2.0", "BUILD-4")
	git(t, author, "commit", "-am", "v1.2.0")
	git(t, author, "push", "origin", "main")
	out = runCepm(t, bin, "update")
	if !strings.Contains(out, "not running") {
		t.Errorf("update should mention Chrome is not running:\n%s", out)
	}

	t.Log("scenario 5+6: on relaunch the host catches up (code pulled while closed) and refreshes the helper")
	// Age the helper marker: the new host must rewrite the files on connect.
	markerPath := filepath.Join(helperDir, ".cepm-helper-version")
	if err := os.WriteFile(markerPath, []byte("0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-load both directories: like a real Chrome restart, this re-reads
	// them from disk (path-derived IDs stay the same).
	if onDisk, err := os.ReadFile(filepath.Join(extDir, "background.js")); err == nil {
		t.Logf("on-disk background.js after closed-Chrome update: %q", string(onDisk))
	}
	chrome, cdp = launchChrome(t, chromeBin, profile, helperDir, extDir)
	waitPing(t, 90*time.Second)
	waitBuild(t, cdp, extID, "BUILD-4", 60*time.Second)
	if got := waitVersion(t, extID, "1.2.0", 30*time.Second); got != "1.2.0" {
		t.Errorf("a Chrome restart should apply the manifest version too (got %q)", got)
	}
	waitFor(t, "helper files refreshed by host", 60*time.Second, func() bool {
		return helperext.InstalledVersion(helperDir) == helperext.Version
	})
	// Refreshing the helper must not cost us the connection (a self-reload
	// would drop the native messaging port for good).
	if _, err := ipc.Ping(context.Background()); err != nil {
		t.Fatalf("host unreachable after the helper refresh: %v", err)
	}

	t.Log("scenario 8: an extension that pins its id with a manifest \"key\" is tracked by that id")
	pinnedDir := filepath.Join(author, "pinned")
	writePinnedExtension(t, pinnedDir)
	git(t, author, "add", "-A")
	git(t, author, "commit", "-m", "add pinned extension")
	git(t, author, "push", "origin", "main")
	runCepm(t, bin, "update")
	// The id must come from the key, not from the path — that is what Chrome
	// uses, and everything (reload, list, doctor, cleanup) matches on it.
	if out := runCepm(t, bin, "list", "--json"); !strings.Contains(out, pinnedExtensionID) {
		t.Errorf("cepm should record the key-derived id %s:\n%s", pinnedExtensionID, out)
	}
	installedPinned := filepath.Join(cepmHome, "repos", "testrepo", "pinned")
	if id, err := cdp.loadUnpacked(installedPinned); err != nil {
		t.Fatalf("loadUnpacked pinned: %v", err)
	} else if id != pinnedExtensionID {
		t.Errorf("Chrome assigned %s but cepm computed %s", id, pinnedExtensionID)
	}

	t.Log("scenario 7: the periodic auto-update applies a push with no CLI interaction")
	writeSWExtension(t, filepath.Join(author, "widget"), "1.2.0", "BUILD-5")
	git(t, author, "commit", "-am", "auto update code")
	git(t, author, "push", "origin", "main")
	waitBuild(t, cdp, extID, "BUILD-5", 2*time.Minute)

	stopChrome(t, chrome)
}

// ---- helpers ----

// workRoot is the one directory this suite creates anything under, so a
// sweep never has to guess whether something in /tmp belongs to it.
const workRoot = "/tmp/cepm-e2e-work"

// markerName holds the pid of the run that created a work directory. A
// directory is only removed when this file is present and readable and that
// process is gone: a name prefix and an age are not proof of ownership, and
// deleting on that basis would eventually delete something a person made.
const markerName = ".cepm-e2e-owner"

// newWorkDir creates a work directory owned by this process.
func newWorkDir(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(workRoot, "run-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, markerName),
		[]byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	// Extension ids are path hashes and Chrome sees the canonical path
	// (/tmp → /private/tmp on macOS).
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// sweepStaleWorkDirs removes work directories whose creating process is gone
// — a run killed before its cleanup could run. Anything it cannot prove it
// owns is reported, not deleted.
func sweepStaleWorkDirs(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		return // nothing here yet
	}
	for _, e := range entries {
		dir := filepath.Join(workRoot, e.Name())
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue // never follow a link out of the work root
		}
		data, err := os.ReadFile(filepath.Join(dir, markerName))
		if err != nil {
			t.Logf("leaving %s alone: no ownership marker", dir)
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Logf("leaving %s alone: unreadable ownership marker", dir)
			continue
		}
		if pid == os.Getpid() || processAlive(pid) {
			continue // ours, or another run still going
		}
		_ = os.RemoveAll(dir)
	}
}

// processAlive reports whether a pid still exists.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// realGoEnv reports the caller's build caches. Call it before HOME is
// replaced.
func realGoEnv(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 2 {
		t.Fatalf("go env returned %q", out)
	}
	return []string{"GOMODCACHE=" + lines[0], "GOCACHE=" + lines[1]}
}

func buildCepm(t *testing.T, home string, goEnv []string) string {
	t.Helper()
	bin := filepath.Join(home, "bin", "cepm")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/cepm")
	cmd.Dir = ".." // repo root
	cmd.Env = append(os.Environ(), goEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cepm: %v\n%s", err, out)
	}
	return bin
}

// removeWorkDir deletes a work directory, including any read-only directory
// a tool may have left behind (removing an entry needs write permission on
// its parent, not on the entry). A failure is reported rather than swallowed:
// each leftover holds a Chrome profile worth ~15MB.
func removeWorkDir(t *testing.T, dir string) {
	t.Helper()
	err := os.RemoveAll(dir)
	if err == nil {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		// A partial removal takes the ownership marker with it, and without
		// it no later run may touch what is left. Put it back so the sweep
		// can finish the job once this process is gone.
		_ = os.WriteFile(filepath.Join(dir, markerName),
			[]byte(strconv.Itoa(os.Getpid())), 0o644)
		t.Errorf("could not remove the work directory %s: %v", dir, err)
	}
}

func runCepm(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	t.Logf("$ cepm %s\n%s", strings.Join(args, " "), out)
	if err != nil {
		t.Fatalf("cepm %v: %v", args, err)
	}
	return string(out)
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeSWExtension writes the test extension. Its service worker exposes a
// build marker so the test can observe which code Chrome is actually running
// — the manifest version alone is not enough, because Chrome keeps the
// manifest it cached at install time until a full reload.
func writeSWExtension(t *testing.T, dir, version, build string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"manifest_version": 3, "name": "E2E Widget", "version": %q,
  "permissions": ["alarms"], "background": {"service_worker": "background.js"}}`, version)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// The alarm keeps waking the worker so the test can query it.
	sw := fmt.Sprintf("self.BUILD = %q;\n"+
		"chrome.alarms.create('keep', {periodInMinutes: 0.5});\n"+
		"chrome.alarms.onAlarm.addListener(() => {});\n", build)
	if err := os.WriteFile(filepath.Join(dir, "background.js"), []byte(sw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pinnedExtensionKey is a throwaway public key (its private half was never
// kept) used to check that cepm derives ids the way Chrome does when a
// manifest pins its id. pinnedExtensionID is the id Chrome must assign.
const (
	pinnedExtensionKey = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA3yuXjwS4vVRxKrFpRZLNmT/GKSGFfTIgfVJwh5yHdhdr+LjANs26YiMlnC7uMI1oZPo5FSvkDlK12py2rkpZ11ADIk0Qav5ZS7TW5ruw83IU6F/rmQtRq5vwASc7AGwBSLiYMJ+XVtFDRCEAr5l7LOkL1jzdJbkb3XbH69ah9CALwKey1xfoQhiKvFL1dEa8/TGd3iXaH7s5v9abON1rCfX3AvvQvekc0HEXnxNWcEduUYRKrCWAuVmyRViES81gmZmY90rmwCrtVBHQmDwBdYqBH7eSF6eE1iazl5cMzDJgk9YxsjQO9VntcO2YJMApdnsxXMMXsyBFZYRCBk+S6wIDAQAB"
	pinnedExtensionID  = "efchjjaogobppnakahbhfpimoophkbog"
)

func writePinnedExtension(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"manifest_version": 3, "name": "E2E Pinned", "version": "1.0.0", "key": %q}`,
		pinnedExtensionKey)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// currentVersion returns the extension version Chrome reports, or a marker.
func currentVersion(t *testing.T, extID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exts, err := ipc.ListChrome(ctx)
	if err != nil {
		return "(host error: " + err.Error() + ")"
	}
	for _, e := range exts {
		if e.ID == extID {
			return e.Version
		}
	}
	return "(not loaded)"
}

// placeNMManifest copies the manifest "cepm setup" wrote (regular-Chrome
// location under the fake HOME) into <profile>/NativeMessagingHosts — the
// directory Chrome for Testing reads instead of the OS-level locations, by
// design, for hermetic tests.
func placeNMManifest(t *testing.T, home, profile string) {
	t.Helper()
	var src string
	if runtime.GOOS == "darwin" {
		src = filepath.Join(home, "Library", "Application Support",
			"Google", "Chrome", "NativeMessagingHosts", nmmanifest.FileName())
	} else {
		src = filepath.Join(home, ".config", "google-chrome", "NativeMessagingHosts", nmmanifest.FileName())
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("setup did not write the NM manifest: %v", err)
	}
	dir := filepath.Join(profile, "NativeMessagingHosts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, nmmanifest.FileName()), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// launchChrome starts Chrome for Testing with a CDP pipe attached. loadDirs
// are installed via Extensions.loadUnpacked — true unpacked installs, the
// same thing the "Load unpacked" button does (and they persist in the
// profile across relaunches, so pass loadDirs only on the first launch).
func launchChrome(t *testing.T, chromeBin, profile string, loadDirs ...string) (*exec.Cmd, *cdpPipe) {
	t.Helper()
	extraFiles, cdp, err := newCDPPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := startChromeWithFiles(t, chromeBin, profile, extraFiles)
	t.Cleanup(func() { cdp.close(); stopChrome(t, cmd) })

	for _, dir := range loadDirs {
		id, err := cdp.loadUnpacked(dir)
		if err != nil {
			stopChrome(t, cmd)
			t.Fatalf("Extensions.loadUnpacked %s: %v", dir, err)
		}
		t.Logf("loaded unpacked %s (id %s)", dir, id)
	}
	return cmd, cdp
}

// waitBuild polls the extension's service worker until it runs the expected
// build marker — the ground truth for "did Chrome pick up the new code".
func waitBuild(t *testing.T, cdp *cdpPipe, extID, want string, timeout time.Duration) {
	t.Helper()
	last := ""
	var lastErr error
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got, err := cdp.evalInServiceWorker(extID, "self.BUILD")
		if err == nil {
			last = got
			if got == want {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("extension never ran %s (last observed %q, last error %v)", want, last, lastErr)
}

// startChromeWithFiles starts Chrome with the CDP pipe fds attached. When no
// args are given the standard test flags are used.
func startChromeWithFiles(t *testing.T, chromeBin, profile string, extraFiles []*os.File, args ...string) *exec.Cmd {
	t.Helper()
	if len(args) == 0 {
		args = chromeArgs(profile)
	}
	cmd := exec.Command(chromeBin, args...)
	cmd.Env = append(os.Environ(), "CEPM_BOOTSTRAP_UPDATE_DELAY=5s")
	cmd.ExtraFiles = extraFiles // fd 3 (commands), fd 4 (responses)
	// Own process group so Chrome's children die with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("launch chrome: %v", err)
	}
	extraFiles[0].Close() // parent copies of Chrome's ends
	extraFiles[1].Close()
	return cmd
}

func chromeArgs(profile string) []string {
	args := []string{
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--remote-debugging-pipe",
		"--enable-unsafe-extension-debugging",
	}
	if os.Getenv("CEPM_E2E_HEADED") == "" {
		args = append(args, "--headless=new")
	}
	if runtime.GOOS == "linux" {
		// CI containers usually cannot set up Chrome's sandbox, and /dev/shm
		// is often tiny there. Safe here: the profile is throwaway.
		args = append(args, "--no-sandbox", "--disable-dev-shm-usage")
	}
	return append(args, "about:blank")
}

func stopChrome(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_, _ = cmd.Process.Wait()
	// Give the native host a moment to notice EOF and release the socket.
	time.Sleep(500 * time.Millisecond)
}

func waitPing(t *testing.T, timeout time.Duration) {
	t.Helper()
	waitFor(t, "native host reachable (helper connected)", timeout, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := ipc.Ping(ctx)
		return err == nil
	})
}

// waitVersion polls Chrome for the extension's version until it matches want
// or the timeout passes, returning the last observed version.
func waitVersion(t *testing.T, extID, want string, timeout time.Duration) string {
	t.Helper()
	last := "(not loaded)"
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		exts, err := ipc.ListChrome(ctx)
		cancel()
		if err == nil {
			for _, e := range exts {
				if e.ID == extID {
					last = e.Version
				}
			}
			if last == want {
				return last
			}
		}
		time.Sleep(time.Second)
	}
	return last
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}
