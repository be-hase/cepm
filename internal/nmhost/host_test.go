package nmhost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/config"
	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
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
	// holdReload, when non-nil, blocks the answer to a reload request until
	// the channel is closed: it opens the window a racing state change would
	// have to slip through.
	holdReload chan struct{}
	reloadSeen chan struct{}
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
			if f.reloadSeen != nil {
				select {
				case f.reloadSeen <- struct{}{}:
				default:
				}
			}
			if f.holdReload != nil {
				<-f.holdReload
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

// seedHostState points CEPM_HOME at a fresh temp dir (short path: unix
// sockets) and registers testrepo with the given extensions.
func seedHostState(t *testing.T, exts ...state.Extension) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	st := state.New()
	st.Repos["testrepo"] = &state.Repo{
		URL: "u", Track: state.TrackBranch, Branch: "main",
		Head:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Extensions: exts,
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
}

// newTestHost wires a Host to helper over in-memory pipes, with the writer
// pump and the read loop running until the test ends. The catch-up flag is
// pre-set: the hello-triggered catch-up reload would race most assertions.
func newTestHost(t *testing.T, helper *fakeHelper) (*Host, context.Context) {
	t.Helper()
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
	h.caughtUp.Store(true)
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
	go func() { _ = h.readLoop(ctx) }()
	helper.t = t
	helper.toHost = helperToHostW
	helper.from = hostToHelperR
	go helper.run()
	return h, ctx
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
	// The owed id has to be live and enabled: pending reloads are re-checked
	// against the state before every attempt.
	seedHostState(t, state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA})
	helper := &fakeHelper{reloads: make(chan []string, 4)}
	h, ctx := newTestHost(t, helper)

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
	seedHostState(t,
		state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA},
		state.Extension{Dir: "b", Name: "B", ID: idB, Key: keyB},
	)
	helper := &fakeHelper{reloads: make(chan []string, 4)}
	h, ctx := newTestHost(t, helper)

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
	st, err := state.Load()
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

// Deciding what to reload and sending it must be one step. Otherwise a
// "cepm disable" can complete in the window between the two, and Chrome is
// told to reload an extension the user just turned off. Holding the update
// lock across both makes the two operations order themselves: this test
// proves the disable cannot commit while a reload is in flight.
func TestReloadHoldsTheLockSoDisableCannotSlipIn(t *testing.T) {
	seedHostState(t, state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA})
	helper := &fakeHelper{
		holdReload: make(chan struct{}),
		reloadSeen: make(chan struct{}, 1),
	}
	h, ctx := newTestHost(t, helper)

	reloadDone := make(chan error, 1)
	go func() {
		_, _, err := h.reloadEnabled(ctx, []string{idA})
		reloadDone <- err
	}()

	select {
	case <-helper.reloadSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("the reload request never reached the helper")
	}

	// The reload is in flight. A disable must not be able to commit now.
	disabled := make(chan error, 1)
	go func() {
		disabled <- updater.WithLock(context.Background(), func() error {
			s, err := state.Load()
			if err != nil {
				return err
			}
			s.Repos["testrepo"].Extensions[0].Disabled = true
			return s.Save()
		})
	}()
	select {
	case err := <-disabled:
		t.Fatalf("disable committed while a reload was in flight (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(helper.holdReload)
	if err := <-reloadDone; err != nil {
		t.Fatalf("reload: %v", err)
	}
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatalf("disable after the reload: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("disable never completed after the reload finished")
	}
}

// The same guarantee for a reload arriving on the control socket (cepm
// reload / cepm update): the handler must take the lock too, or the window
// reopens for every CLI-driven reload.
func TestSocketReloadHoldsTheLockToo(t *testing.T) {
	seedHostState(t, state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA})
	helper := &fakeHelper{
		holdReload: make(chan struct{}),
		reloadSeen: make(chan struct{}, 1),
	}
	h, ctx := newTestHost(t, helper)

	respDone := make(chan ipc.Response, 1)
	go func() {
		respDone <- h.handleIPC(ctx, ipc.Request{Cmd: ipc.CmdReload, IDs: []string{idA}})
	}()
	select {
	case <-helper.reloadSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("the reload request never reached the helper")
	}

	disabled := make(chan error, 1)
	go func() {
		disabled <- updater.WithLock(context.Background(), func() error {
			s, err := state.Load()
			if err != nil {
				return err
			}
			s.Repos["testrepo"].Extensions[0].Disabled = true
			return s.Save()
		})
	}()
	select {
	case err := <-disabled:
		t.Fatalf("disable committed during a socket reload (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(helper.holdReload)
	if resp := <-respDone; !resp.OK {
		t.Fatalf("reload response: %+v", resp)
	}
	if err := <-disabled; err != nil {
		t.Fatalf("disable after the reload: %v", err)
	}
}

// When the state changes between accepting a reload request and acting on
// it, the answer must say so — and must not borrow "skipped_disabled",
// which means Chrome has the extension switched off. Reproduced by holding
// the update lock so the handler blocks after authorizing, and disabling the
// extension inside that window.
func TestReloadReportsStateChangeDistinctlyFromChromeDisabled(t *testing.T) {
	seedHostState(t, state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA})
	helper := &fakeHelper{reloads: make(chan []string, 4)}
	h, ctx := newTestHost(t, helper)

	// Hold the lock, let the request authorize and block on it, then change
	// the state from inside the window. The hook — not a sleep — is what
	// makes "the handler is past authorization" observable.
	authorized := make(chan struct{})
	h.afterReloadAuthorized = func() { close(authorized) }
	respDone := make(chan ipc.Response, 1)
	err := updater.WithLock(context.Background(), func() error {
		go func() {
			respDone <- h.handleIPC(ctx, ipc.Request{Cmd: ipc.CmdReload, IDs: []string{idA}})
		}()
		select {
		case <-authorized:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("the handler never got past authorization")
		}
		s, err := state.Load()
		if err != nil {
			return err
		}
		s.Repos["testrepo"].Extensions[0].Disabled = true
		return s.Save()
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case resp := <-respDone:
		if !resp.OK || len(resp.Results) != 1 {
			t.Fatalf("unexpected response: %+v", resp)
		}
		got := resp.Results[0].Status
		if got == ipc.StatusSkippedDisabled {
			t.Error(`a cepm-side state change must not be reported as "turned off in Chrome"`)
		}
		if got != ipc.StatusSkippedStateChanged {
			t.Errorf("status = %q, want %q", got, ipc.StatusSkippedStateChanged)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reload request never got an answer")
	}
	select {
	case ids := <-helper.reloads:
		t.Errorf("nothing should have been sent to Chrome, got %v", ids)
	default:
	}
}

// A panic while handling one CLI request must answer that request with an
// error, not kill the host for every other caller: these paths act on state
// and Chrome, which is exactly where a panic is most likely to live.
func TestHandleIPCRecoversFromAPanic(t *testing.T) {
	seedHostState(t, state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA})

	h := &Host{
		version:   "test",
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		out:       make(chan []byte, 16),
		pending:   map[string]chan json.RawMessage{},
		startedAt: time.Now(),
	}
	h.afterReloadAuthorized = func() { panic("kaboom") }

	resp := h.handleIPC(context.Background(), ipc.Request{Cmd: ipc.CmdReload, IDs: []string{idA}})
	if resp.OK || resp.Error == "" {
		t.Errorf("a panicking request should answer with an error, got %+v", resp)
	}
	if resp := h.handleIPC(context.Background(), ipc.Request{Cmd: ipc.CmdPing}); !resp.OK {
		t.Errorf("the host should keep serving after a recovered panic, got %+v", resp)
	}
}

// Chrome discards the host's stderr, so a panic must land in the log and
// then take down only what cannot run without the goroutine that died: an
// essential one shuts the host down cleanly, a request-scoped one does not.
func TestSafegoShutsDownOnEssentialPanicOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Host{log: slog.New(slog.NewTextHandler(io.Discard, nil)), cancel: cancel}

	ran := make(chan struct{})
	h.safego("non-essential", false, func() { defer close(ran); panic("boom") })
	<-ran
	select {
	case <-ctx.Done():
		t.Fatal("a non-essential panic must not shut the host down")
	case <-time.After(300 * time.Millisecond):
	}

	h.safego("essential", true, func() { panic("boom") })
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("an essential panic must shut the host down")
	}
}

// syncBuffer is an io.Writer safe to read while the scheduler goroutine
// writes log lines to it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// schedHarness drives runScheduler deterministically: every wait the
// scheduler requests comes out of delays (the cycle boundary), ticks go in
// through tick, and every update a cycle would run lands in updates — no
// real time passes and no sleep synchronizes anything.
type schedHarness struct {
	h       *Host
	buf     *syncBuffer
	tick    chan time.Time
	delays  chan time.Duration
	updates chan *config.Config
	cancel  context.CancelFunc
	done    chan struct{}
	cfgPath string
}

func startScheduler(t *testing.T, initialConfig string) *schedHarness {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	sh := &schedHarness{
		buf:     &syncBuffer{},
		tick:    make(chan time.Time),
		delays:  make(chan time.Duration, 64),
		updates: make(chan *config.Config, 64),
		done:    make(chan struct{}),
		cfgPath: filepath.Join(dir, "config.toml"),
	}
	if initialConfig != "" {
		sh.writeConfig(t, initialConfig)
	}
	sh.h = &Host{log: slog.New(slog.NewTextHandler(sh.buf, nil))}
	sh.h.schedulerWait = func(d time.Duration) <-chan time.Time {
		sh.delays <- d
		return sh.tick
	}
	sh.h.runUpdate = func(_ context.Context, cfg *config.Config) { sh.updates <- cfg }
	ctx, cancel := context.WithCancel(context.Background())
	sh.cancel = cancel
	t.Cleanup(cancel)
	go func() { sh.h.runScheduler(ctx); close(sh.done) }()
	// Consume the bootstrap wait so the first cycle() tick lands cleanly.
	select {
	case <-sh.delays:
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler never requested its first wait")
	}
	return sh
}

func (sh *schedHarness) writeConfig(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(sh.cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cycle delivers one tick and returns the delay the scheduler chose for its
// next wait; when it returns, the cycle's update (if any) is already in
// updates, because the scheduler requests the next wait only afterwards.
func (sh *schedHarness) cycle(t *testing.T) time.Duration {
	t.Helper()
	select {
	case sh.tick <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler never returned to its wait")
	}
	select {
	case d := <-sh.delays:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler never requested the next wait")
	}
	return 0
}

func (sh *schedHarness) mustUpdate(t *testing.T) *config.Config {
	t.Helper()
	select {
	case cfg := <-sh.updates:
		return cfg
	default:
		t.Fatal("expected an update this cycle")
		return nil
	}
}

func (sh *schedHarness) noUpdate(t *testing.T) {
	t.Helper()
	select {
	case cfg := <-sh.updates:
		t.Fatalf("no update expected this cycle, got one with %+v", cfg)
	default:
	}
}

func inJitterBand(d, interval time.Duration) bool {
	jitter := time.Duration(float64(interval) * 0.1)
	return d >= interval-jitter && d <= interval+jitter
}

// A config.toml that does not parse must pause the scheduler — no updates,
// no busy loop, no repeated identical complaints — and a repaired file must
// bring it back without a Chrome restart.
func TestSchedulerRecoversFromABrokenConfig(t *testing.T) {
	sh := startScheduler(t, "[update\n")

	d := sh.cycle(t)
	sh.noUpdate(t)
	if d != configRecheckInterval {
		t.Errorf("invalid config should pace rechecks at %v, got %v", configRecheckInterval, d)
	}
	if d <= 0 {
		t.Fatal("a non-positive recheck delay would busy-loop")
	}
	sh.cycle(t)
	sh.noUpdate(t)
	if n := strings.Count(sh.buf.String(), "periodic updates paused"); n != 1 {
		t.Errorf("an unchanged config error should be logged once, got %d times", n)
	}

	sh.writeConfig(t, "[update]\nauto = true\ninterval = \"90m\"\n")
	d = sh.cycle(t)
	sh.mustUpdate(t)
	if !strings.Contains(sh.buf.String(), "auto update resumed") {
		t.Error("recovery should be logged")
	}
	if !inJitterBand(d, 90*time.Minute) {
		t.Errorf("delay %v outside 90m ±10%%", d)
	}
}

// auto = false pauses updates but keeps watching the file: turning it back
// on (or off again) applies without a Chrome restart.
func TestSchedulerFollowsAutoToggles(t *testing.T) {
	sh := startScheduler(t, "[update]\nauto = false\n")

	if d := sh.cycle(t); d != configRecheckInterval {
		t.Errorf("auto=false should pace rechecks at %v, got %v", configRecheckInterval, d)
	}
	sh.noUpdate(t)

	sh.writeConfig(t, "[update]\nauto = true\n")
	sh.cycle(t)
	sh.mustUpdate(t)

	sh.writeConfig(t, "[update]\nauto = false\n")
	if d := sh.cycle(t); d != configRecheckInterval {
		t.Errorf("turning auto off should pause again, got delay %v", d)
	}
	sh.noUpdate(t)
}

// interval and stash_dirty edits reach the very next cycle.
func TestSchedulerAppliesIntervalAndStashDirtyChanges(t *testing.T) {
	sh := startScheduler(t, "[update]\nauto = true\ninterval = \"60m\"\n")

	d := sh.cycle(t)
	if cfg := sh.mustUpdate(t); cfg.Git.StashDirty {
		t.Error("stash_dirty should start false")
	}
	if !inJitterBand(d, 60*time.Minute) {
		t.Errorf("delay %v outside 60m ±10%%", d)
	}

	sh.writeConfig(t, "[update]\nauto = true\ninterval = \"120m\"\n[git]\nstash_dirty = true\n")
	d = sh.cycle(t)
	cfg := sh.mustUpdate(t)
	if !cfg.Git.StashDirty {
		t.Error("the stash_dirty change should reach the next update")
	}
	if cfg.Update.Interval != 120*time.Minute {
		t.Errorf("interval = %v, want 120m", cfg.Update.Interval)
	}
	if !inJitterBand(d, 120*time.Minute) {
		t.Errorf("delay %v outside 120m ±10%%", d)
	}
}

// No config file at all means defaults — updates run, nothing is flagged —
// and a context cancel ends the scheduler promptly even mid-wait.
func TestSchedulerDefaultsAndCancel(t *testing.T) {
	sh := startScheduler(t, "")
	sh.cycle(t)
	sh.mustUpdate(t)

	sh.cancel()
	select {
	case <-sh.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler did not stop on cancel")
	}
}

// A catch-up reload that fails for one extension — a per-id error status or
// a missing answer, not a transport error — must stay owed and be retried
// on the next tick: it used to be written off on transport success, leaving
// that extension running stale code for the whole session.
func TestCatchUpReloadKeepsIndividualFailuresOwed(t *testing.T) {
	seedHostState(t,
		state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA},
		state.Extension{Dir: "b", Name: "B", ID: idB, Key: keyB},
	)
	helper := &fakeHelper{reloads: make(chan []string, 4)}
	h, ctx := newTestHost(t, helper)

	// The helper answers every reload with a per-id error.
	helper.failReloads.Store(true)
	h.caughtUp.Store(false)
	h.catchUpReload(ctx)
	select {
	case <-helper.reloads:
	case <-time.After(10 * time.Second):
		t.Fatal("the catch-up reload never reached the helper")
	}
	h.pendingReloadMu.Lock()
	owed := len(h.pendingReload)
	h.pendingReloadMu.Unlock()
	if owed != 2 {
		t.Fatalf("individually failed catch-up reloads must stay owed, got %d owed", owed)
	}

	// The user disables A before the retry; only B may be retried, and a
	// healthy helper settles it.
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Repos["testrepo"].Extensions[0].Disabled = true
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	helper.failReloads.Store(false)
	h.flushPendingReloads(ctx) // the next scheduler tick does exactly this
	select {
	case ids := <-helper.reloads:
		if len(ids) != 1 || ids[0] != idB {
			t.Errorf("the retry should carry only the still-enabled id, got %v", ids)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the owed catch-up reload was never retried")
	}
	h.pendingReloadMu.Lock()
	defer h.pendingReloadMu.Unlock()
	if len(h.pendingReload) != 0 {
		t.Errorf("settled and dropped debts must not linger: %v", h.pendingReload)
	}
}
