package nmhost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/config"
	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/state"
)

// fakeHelper emulates the helper extension's side of the native messaging
// port: it answers hello acks are ignored, pings with pongs, and reload/list
// requests with canned results.
type fakeHelper struct {
	t       *testing.T
	toHost  io.Writer
	from    io.Reader
	reloads chan []string // receives extensionIds of every reload request (optional)
	// failReloads makes reload requests answer status "error" while set —
	// the transient helper failure the pending-reload retry exists for.
	failReloads atomic.Bool
}

func (f *fakeHelper) send(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		f.t.Error(err)
		return
	}
	if err := WriteMessage(f.toHost, b); err != nil && f.t != nil {
		return // host likely shut down; the test is ending
	}
}

func (f *fakeHelper) run() {
	f.send(map[string]any{"type": "hello", "helperVersion": "0.1.0"})
	for {
		frame, err := ReadMessage(f.from)
		if err != nil {
			return
		}
		var msg struct {
			Type         string   `json:"type"`
			Seq          int64    `json:"seq"`
			RequestID    string   `json:"requestId"`
			ExtensionIDs []string `json:"extensionIds"`
			ExtensionID  string   `json:"extensionId"`
		}
		if err := json.Unmarshal(frame, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "ping":
			f.send(map[string]any{"type": "pong", "seq": msg.Seq})
		case "reload":
			if f.reloads != nil {
				f.reloads <- msg.ExtensionIDs
			}
			status := "reloaded"
			if f.failReloads.Load() {
				status = "error"
			}
			results := make([]map[string]string, len(msg.ExtensionIDs))
			for i, id := range msg.ExtensionIDs {
				results[i] = map[string]string{"id": id, "status": status}
			}
			f.send(map[string]any{"type": "reloadResult", "requestId": msg.RequestID, "results": results})
		case "listExtensions":
			f.send(map[string]any{
				"type": "listResult", "requestId": msg.RequestID,
				"extensions": []map[string]any{
					{"id": idA, "name": "Fake Ext", "version": "1.0", "enabled": true},
				},
			})
		case "uninstall":
			// Emulate the user confirming Chrome's dialog.
			f.send(map[string]any{
				"type": "uninstallResult", "requestId": msg.RequestID,
				"status": "uninstalled",
			})
		}
	}
}

// Fixture ids pinned by manifest keys, because state.Validate re-derives
// every live id from its recorded key or path.
var (
	keyA, idA = fixtureKey("seed-a")
	keyB, idB = fixtureKey("seed-b")
)

func fixtureKey(seed string) (key, id string) {
	return base64.StdEncoding.EncodeToString([]byte(seed)),
		extid.FromPublicKey([]byte(seed))
}

func TestHostEndToEnd(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)

	// The host only acts on extensions cepm manages, so register them.
	st := state.New()
	st.Repos["testrepo"] = &state.Repo{
		URL: "git@example.com:t/r.git", Track: state.TrackBranch, Branch: "main", Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Extensions: []state.Extension{
			{Dir: "a", Name: "A", ID: idA, Key: keyA},
			{Dir: "b", Name: "B", ID: idB, Key: keyB},
		},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	// chromeOut: host's "stdin" written by the fake helper.
	// chromeIn: host's "stdout" read by the fake helper.
	helperToHostR, helperToHostW := io.Pipe()
	hostToHelperR, hostToHelperW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() {
		runDone <- RunIO(ctx, "test", helperToHostR, hostToHelperW)
	}()
	helper := &fakeHelper{t: t, toHost: helperToHostW, from: hostToHelperR}
	go helper.run()

	// Wait for the host to win leadership and open the control socket.
	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pingCancel()
	info, err := ipc.Ping(pingCtx)
	if err != nil {
		t.Fatalf("ping host: %v", err)
	}
	if !info.Leader || info.Version != "test" {
		t.Errorf("unexpected host info: %+v", info)
	}

	results, err := ipc.Reload(ctx, []string{idA, idB})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != ipc.StatusReloaded {
		t.Errorf("unexpected reload results: %+v", results)
	}

	exts, err := ipc.ListChrome(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) != 1 || exts[0].Name != "Fake Ext" {
		t.Errorf("unexpected extensions: %+v", exts)
	}

	status, err := ipc.Uninstall(ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	if status != ipc.StatusUninstalled {
		t.Errorf("uninstall status = %q, want %q", status, ipc.StatusUninstalled)
	}

	// The helper can disable or remove *any* installed extension, so the host
	// must refuse ids cepm does not manage — the socket is reachable by any
	// process running as this user.
	if _, err := ipc.Reload(ctx, []string{"unmanagedunmanagedunmanagedunma"}); err == nil {
		t.Error("reload of an unmanaged extension should be refused")
	}
	if _, err := ipc.Uninstall(ctx, "unmanagedunmanagedunmanagedunma"); err == nil {
		t.Error("uninstall of an unmanaged extension should be refused")
	}

	// Closing the "stdin" pipe simulates Chrome quitting: the host must exit
	// cleanly and release the socket.
	helperToHostW.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("host exit error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host did not exit after stdin EOF")
	}
}

// Updates pulled while Chrome was closed are on disk but Chrome may still run
// the code it cached, so the host reloads managed extensions once on connect.
// Without this assertion the whole catch-up path could be deleted and every
// other unit test would still pass.
func TestHostCatchUpReloadsEnabledExtensionsOnly(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)

	st := state.New()
	st.Repos["testrepo"] = &state.Repo{
		URL: "git@example.com:t/r.git", Track: state.TrackBranch, Branch: "main", Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Extensions: []state.Extension{
			{Dir: "on", Name: "On", ID: idA, Key: keyA},
			{Dir: "off", Name: "Off", ID: idB, Key: keyB, Disabled: true},
		},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	helperToHostR, helperToHostW := io.Pipe()
	hostToHelperR, hostToHelperW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = RunIO(ctx, "test", helperToHostR, hostToHelperW) }()
	helper := &fakeHelper{t: t, toHost: helperToHostW, from: hostToHelperR,
		reloads: make(chan []string, 4)}
	go helper.run()

	select {
	case ids := <-helper.reloads:
		if len(ids) != 1 || ids[0] != idA {
			t.Errorf("catch-up should reload only enabled extensions, got %v", ids)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host never performed the catch-up reload")
	}
	helperToHostW.Close()
}

// After a cepm upgrade the host binary carries newer helper files than the
// ones loaded in Chrome; on connect it must rewrite ~/.cepm/helper. It must
// NOT ask the helper to reload itself: chrome.runtime.reload() tears down
// this port and Chrome does not reliably bring the helper back, which would
// strand the connection until the next Chrome start.
func TestHostRefreshesOutdatedHelper(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)

	// Install helper files, then age the version marker.
	helperDir := filepath.Join(dir, "helper")
	if err := helperext.Install(helperDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helperDir, ".cepm-helper-version"), []byte("0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	helperToHostR, helperToHostW := io.Pipe()
	hostToHelperR, hostToHelperW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() { runDone <- RunIO(ctx, "test", helperToHostR, hostToHelperW) }()
	helper := &fakeHelper{t: t, toHost: helperToHostW, from: hostToHelperR,
		reloads: make(chan []string, 4)}
	go helper.run()

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	if _, err := ipc.Ping(waitCtx); err != nil {
		t.Fatalf("host not reachable: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for helperext.InstalledVersion(helperDir) != helperext.Version && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if v := helperext.InstalledVersion(helperDir); v != helperext.Version {
		t.Errorf("helper files not refreshed: marker=%q, want %q", v, helperext.Version)
	}
	select {
	case ids := <-helper.reloads:
		for _, id := range ids {
			if id == helperext.ExtensionID() {
				t.Error("host must not ask the helper to reload itself (it would drop the port for good)")
			}
		}
	case <-time.After(time.Second):
		// No reload at all is the expected case here: no extensions are
		// registered, so the startup catch-up pass has nothing to do.
	}
	// The connection must have survived the refresh.
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()
	if _, err := ipc.Ping(pingCtx); err != nil {
		t.Errorf("host unreachable after the helper refresh: %v", err)
	}

	helperToHostW.Close()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("host did not exit after stdin EOF")
	}
}

// A reload that fails after a successful auto update must stay owed: the
// checkout has already happened, and forgetting the reload would leave the
// user running old code until the next commit or a Chrome restart. The next
// scheduler tick — even one that finds nothing new — retries it.
func TestAutoUpdateRetriesFailedReloads(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	// The owed id has to be live and enabled: pending reloads are re-checked
	// against the state before every attempt.
	st := state.New()
	st.Repos["testrepo"] = &state.Repo{
		URL: "u", Track: state.TrackBranch, Branch: "main", Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Extensions: []state.Extension{{Dir: "a", Name: "A", ID: idA, Key: keyA}},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	helperToHostR, helperToHostW := io.Pipe()
	hostToHelperR, hostToHelperW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := &Host{
		version:   "test",
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		in:        helperToHostR,
		out:       make(chan []byte, 16),
		pending:   map[string]chan json.RawMessage{},
		startedAt: time.Now(),
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-h.out:
				if err := WriteMessage(hostToHelperW, frame); err != nil {
					return
				}
			}
		}
	}()
	// The hello-triggered catch-up reload would race these assertions.
	h.caughtUp.Store(true)
	go func() { _ = h.readLoop(ctx) }()
	helper := &fakeHelper{t: t, toHost: helperToHostW, from: hostToHelperR,
		reloads: make(chan []string, 4)}
	go helper.run()

	// Tick 1: the update itself succeeds (nothing registered, nothing to
	// pull) but the reload of an owed id fails transiently.
	helper.failReloads.Store(true)
	h.addPendingReloads([]string{idA})
	h.autoUpdate(ctx, &config.Config{})
	select {
	case ids := <-helper.reloads:
		if len(ids) != 1 || ids[0] != idA {
			t.Fatalf("first attempt should carry the owed id, got %v", ids)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no reload attempt reached the helper")
	}

	// Tick 2: up to date again — the owed reload must be retried, and a
	// healthy helper settles it.
	helper.failReloads.Store(false)
	h.autoUpdate(ctx, &config.Config{})
	select {
	case ids := <-helper.reloads:
		if len(ids) != 1 || ids[0] != idA {
			t.Fatalf("the retry should carry the owed id, got %v", ids)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the owed reload was never retried")
	}

	// Settled: a third tick owes nothing.
	h.autoUpdate(ctx, &config.Config{})
	select {
	case ids := <-helper.reloads:
		t.Errorf("a settled reload must not be retried, got %v", ids)
	case <-time.After(500 * time.Millisecond):
	}
}

// An owed reload is a promise made against an old state: if the user
// disables or uninstalls the extension before the retry, delivering it
// anyway would override that choice — the debt is dropped instead.
func TestPendingReloadDropsDisabledAndUninstalledExtensions(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	st := state.New()
	st.Repos["testrepo"] = &state.Repo{
		URL: "u", Track: state.TrackBranch, Branch: "main", Head: head,
		Extensions: []state.Extension{
			{Dir: "a", Name: "A", ID: idA, Key: keyA},
			{Dir: "b", Name: "B", ID: idB, Key: keyB},
		},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	helperToHostR, helperToHostW := io.Pipe()
	hostToHelperR, hostToHelperW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &Host{
		version:   "test",
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		in:        helperToHostR,
		out:       make(chan []byte, 16),
		pending:   map[string]chan json.RawMessage{},
		startedAt: time.Now(),
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-h.out:
				if err := WriteMessage(hostToHelperW, frame); err != nil {
					return
				}
			}
		}
	}()
	// The hello-triggered catch-up reload would race these assertions.
	h.caughtUp.Store(true)
	go func() { _ = h.readLoop(ctx) }()
	helper := &fakeHelper{t: t, toHost: helperToHostW, from: hostToHelperR,
		reloads: make(chan []string, 4)}
	go helper.run()

	// Both reloads fail transiently and stay owed.
	helper.failReloads.Store(true)
	h.addPendingReloads([]string{idA, idB})
	h.flushPendingReloads(ctx)
	select {
	case <-helper.reloads:
	case <-time.After(10 * time.Second):
		t.Fatal("no reload attempt reached the helper")
	}

	// Before the retry: the user disables A and uninstalls B's repository
	// entry entirely.
	st, err = state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Repos["testrepo"].Extensions[0].Disabled = true
	st.Repos["testrepo"].Extensions = st.Repos["testrepo"].Extensions[:1]
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	helper.failReloads.Store(false)
	h.flushPendingReloads(ctx)
	select {
	case ids := <-helper.reloads:
		t.Errorf("neither a disabled nor an uninstalled extension may be reloaded, got %v", ids)
	case <-time.After(500 * time.Millisecond):
	}
	h.pendingReloadMu.Lock()
	defer h.pendingReloadMu.Unlock()
	if len(h.pendingReload) != 0 {
		t.Errorf("dropped debts must not linger, got %v", h.pendingReload)
	}
}
