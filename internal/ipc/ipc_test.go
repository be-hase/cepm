package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startTestServer(t *testing.T, h Handler) {
	t.Helper()
	// Unix socket paths are length-limited (~104 bytes on macOS); use a short
	// tmp dir instead of t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "cepm-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}

	l, err := Listen(filepath.Join(dir, "run", "cepm.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Serve(ctx, l, h)
}

func TestPingRoundTrip(t *testing.T) {
	startTestServer(t, func(ctx context.Context, req Request) Response {
		if req.Cmd != CmdPing {
			return Response{Error: "unexpected cmd"}
		}
		return Response{OK: true, Host: &HostInfo{Version: "test", PID: 42, Leader: true}}
	})
	info, err := Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.PID != 42 || !info.Leader {
		t.Errorf("unexpected host info: %+v", info)
	}
}

func TestReloadRoundTrip(t *testing.T) {
	startTestServer(t, func(ctx context.Context, req Request) Response {
		results := make([]ReloadResult, len(req.IDs))
		for i, id := range req.IDs {
			results[i] = ReloadResult{ID: id, Status: StatusReloaded}
		}
		return Response{OK: true, Results: results}
	})
	results, err := Reload(context.Background(), []string{"aaa", "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ID != "aaa" || results[1].Status != StatusReloaded {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestHostErrorSurfaces(t *testing.T) {
	startTestServer(t, func(ctx context.Context, req Request) Response {
		return Response{Error: "helper not connected"}
	})
	if _, err := Reload(context.Background(), []string{"aaa"}); err == nil {
		t.Error("expected error from host")
	}
}

// A crashed host leaves its socket file behind (only a clean Close unlinks
// it), and the next leader must be able to bind anyway — otherwise one crash
// makes every successor invisible to the CLI for the rest of the session.
func TestListenReplacesAStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "cepm.sock")

	l1, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l1.(*net.UnixListener).SetUnlinkOnClose(false)
	l1.Close() // the file stays: this is what a crash leaves
	if _, err := os.Stat(sock); err != nil {
		t.Fatal("fixture: the stale socket file should still exist")
	}

	l2, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen must replace a stale socket: %v", err)
	}
	defer l2.Close()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("the replaced socket should accept connections: %v", err)
	}
	conn.Close()
}

// A malformed request line must be answered with an error, not swallowed
// with a closed connection: the client would otherwise report a read error
// with no hint of what went wrong.
func TestServeAnswersMalformedRequestsWithAnError(t *testing.T) {
	startTestServer(t, func(ctx context.Context, req Request) Response {
		t.Error("the handler must not run for a malformed request")
		return Response{}
	})
	sock := filepath.Join(os.Getenv("CEPM_HOME"), "run", "cepm.sock")
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "{this is not json\n")
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("expected an error response, got read error: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, line)
	}
	if resp.OK || !strings.Contains(resp.Error, "invalid request") {
		t.Errorf("expected an invalid-request error, got %+v", resp)
	}
}

// An uninstall waits minutes for the user to answer Chrome's dialog; a ping
// arriving meanwhile must still be answered, or a busy host is
// indistinguishable from a dead one.
func TestABlockedRequestDoesNotBlockOtherConnections(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	startTestServer(t, func(ctx context.Context, req Request) Response {
		if req.Cmd == CmdUninstall {
			entered <- struct{}{}
			<-release
			return Response{OK: true, Status: StatusUninstalled}
		}
		return Response{OK: true, Host: &HostInfo{Version: "test"}}
	})

	uninstallDone := make(chan error, 1)
	go func() {
		_, err := Uninstall(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		uninstallDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the uninstall request never reached the handler")
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Ping(pingCtx); err != nil {
		t.Fatalf("ping was blocked behind the in-flight uninstall: %v", err)
	}

	close(release)
	if err := <-uninstallDone; err != nil {
		t.Fatalf("the released uninstall should complete: %v", err)
	}
}

func TestDialFailsFastWhenNoHost(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled context → no retry loop
	if _, err := Ping(ctx); err == nil {
		t.Error("expected ErrHostNotRunning")
	}
}
