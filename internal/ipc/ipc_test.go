package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
		if req.Cmd == CmdPing {
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
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
		if req.Cmd == CmdPing {
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
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
		return Response{OK: true, Host: &HostInfo{Version: "test", Protocol: ProtocolVersion}}
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

// A host old enough to predate the protocol field ignores it as unknown
// JSON and would carry out whatever it is sent, so the refusal has to
// happen on this side: the operation must never leave the CLI. Ping is
// exempt — it is how the mismatch is discovered at all.
func TestOperationsAreRefusedAgainstAMismatchedHost(t *testing.T) {
	for label, hostProtocol := range map[string]int{
		"pre-protocol host": 0,
		"future host":       ProtocolVersion + 1,
	} {
		t.Run(label, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			startTestServer(t, func(ctx context.Context, req Request) Response {
				mu.Lock()
				seen = append(seen, req.Cmd)
				mu.Unlock()
				if req.Cmd == CmdPing {
					return Response{OK: true, Host: &HostInfo{Version: "old", Protocol: hostProtocol}}
				}
				// What an old host does: acts, having ignored the field.
				return Response{OK: true, Results: []ReloadResult{{ID: "aaa", Status: StatusReloaded}},
					Extensions: []ChromeExt{{ID: "aaa"}}, Status: StatusUninstalled}
			})

			if _, err := Reload(context.Background(), []string{"aaa"}); !errors.Is(err, ErrProtocolMismatch) {
				t.Errorf("reload should be refused as a protocol mismatch, got %v", err)
			}
			if _, err := ListChrome(context.Background()); !errors.Is(err, ErrProtocolMismatch) {
				t.Errorf("list should be refused as a protocol mismatch, got %v", err)
			}
			if _, err := Uninstall(context.Background(), "aaa"); !errors.Is(err, ErrProtocolMismatch) {
				t.Errorf("uninstall should be refused as a protocol mismatch, got %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			for _, cmd := range seen {
				if cmd != CmdPing {
					t.Errorf("no operation may reach a mismatched host, but %q was sent (all: %v)", cmd, seen)
				}
			}
			if len(seen) == 0 {
				t.Error("the preflight ping should still have been sent")
			}
		})
	}
}

// A matching host receives the operation exactly once (the preflight ping
// must not turn into a second operation or a retry).
func TestMatchingProtocolSendsTheOperationOnce(t *testing.T) {
	var mu sync.Mutex
	var operations int
	startTestServer(t, func(ctx context.Context, req Request) Response {
		if req.Cmd == CmdPing {
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
		mu.Lock()
		operations++
		mu.Unlock()
		return Response{OK: true, Results: []ReloadResult{{ID: "aaa", Status: StatusReloaded}}}
	})
	if _, err := Reload(context.Background(), []string{"aaa"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if operations != 1 {
		t.Errorf("the operation should be sent exactly once, got %d", operations)
	}
}

// The handshake and the command must reach the same host process. With two
// connections a host can quit in between and a follower from before the
// upgrade can win leadership and receive a command this CLI only ever
// verified against its predecessor — so the command must ride the same
// connection the ping was answered on.
func TestOperationsSurviveALeaderHandoffAfterTheHandshake(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "run", "cepm.sock")

	// The current host: answers the handshake, then dies — the moment a
	// two-connection client would step into.
	handedOver := make(chan struct{})
	current, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	currentCtx, stopCurrent := context.WithCancel(context.Background())
	t.Cleanup(stopCurrent)
	go Serve(currentCtx, current, func(ctx context.Context, req Request) Response {
		if req.Cmd == CmdPing {
			// Hand the socket to the old host as soon as the handshake is
			// answered; the reply is already on its way back.
			defer func() {
				stopCurrent()
				<-handedOver
			}()
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
		return Response{OK: true, Results: []ReloadResult{{ID: "aaa", Status: StatusReloaded}}}
	})

	// The successor: a host from before the protocol field, which would
	// carry out anything it is handed.
	var mu sync.Mutex
	var oldHostSaw []string
	go func() {
		<-currentCtx.Done()
		old, err := Listen(sock) // replaces the socket, like a new leader
		if err != nil {
			close(handedOver)
			return
		}
		t.Cleanup(func() { old.Close() })
		go Serve(context.Background(), old, func(ctx context.Context, req Request) Response {
			mu.Lock()
			oldHostSaw = append(oldHostSaw, req.Cmd)
			mu.Unlock()
			return Response{OK: true, Results: []ReloadResult{{ID: "aaa", Status: StatusReloaded}}}
		})
		close(handedOver)
	}()

	// Whether this errors or succeeds depends on timing; what must never
	// happen is the old host executing the command.
	_, _ = Reload(context.Background(), []string{"aaa"})
	<-handedOver

	mu.Lock()
	defer mu.Unlock()
	for _, cmd := range oldHostSaw {
		if cmd != CmdPing {
			t.Errorf("the pre-protocol host executed %q after the handoff (saw %v)", cmd, oldHostSaw)
		}
	}
}

// A standalone ping must not leave the connection (or a goroutine) behind.
func TestPingClosesItsConnection(t *testing.T) {
	startTestServer(t, func(ctx context.Context, req Request) Response {
		return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
	})
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		if _, err := Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Connections are closed on return, so the server's per-connection
	// goroutines finish; allow a moment for the scheduler, with a deadline.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutines grew from %d to %d across 20 pings", before, runtime.NumGoroutine())
}
