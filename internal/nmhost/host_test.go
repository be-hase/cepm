package nmhost

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
)

// fakeHelper emulates the helper extension's side of the native messaging
// port: it answers hello acks are ignored, pings with pongs, and reload/list
// requests with canned results.
type fakeHelper struct {
	t       *testing.T
	toHost  io.Writer
	from    io.Reader
	reloads chan []string // receives extensionIds of every reload request (optional)
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
			results := make([]map[string]string, len(msg.ExtensionIDs))
			for i, id := range msg.ExtensionIDs {
				results[i] = map[string]string{"id": id, "status": "reloaded"}
			}
			f.send(map[string]any{"type": "reloadResult", "requestId": msg.RequestID, "results": results})
		case "listExtensions":
			f.send(map[string]any{
				"type": "listResult", "requestId": msg.RequestID,
				"extensions": []map[string]any{
					{"id": "aaaa", "name": "Fake Ext", "version": "1.0", "enabled": true},
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

func TestHostEndToEnd(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)

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

	results, err := ipc.Reload(ctx, []string{"aaaa", "bbbb"})
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

	status, err := ipc.Uninstall(ctx, "aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if status != ipc.StatusUninstalled {
		t.Errorf("uninstall status = %q, want %q", status, ipc.StatusUninstalled)
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
