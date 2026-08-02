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

// startTestServer runs a server for the test and returns a function that
// stops it and waits for Serve to return — a test that swaps a package
// variable the server reads must know when the last reader is gone.
func startTestServer(t *testing.T, h Handler) (stop func()) {
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
	done := make(chan struct{})
	go func() { defer close(done); Serve(ctx, l, h) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
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

// handoffResult is what one run of the handoff scenario observed.
type handoffResult struct {
	oldHostSaw   []string // commands the pre-protocol successor received
	successorErr error    // why the successor never took the socket, if so
}

// runHandoffScenario stages a leader handoff in the middle of a protocol
// handshake: the current host answers the ping and quits, a host from
// before the protocol field takes the socket, and a Reload is attempted
// across the seam. listen builds the successor's listener, so a test can
// inject a startup failure and check the scenario reports it rather than
// silently concluding that nothing reached the old host.
//
// Everything it creates — temp dir, listeners, goroutines — is cleaned up
// before it returns, on both the normal and the failure path. CEPM_HOME is
// the exception: t.Setenv restores it when the test itself ends, which is
// soon enough because nothing outside the test reads it.
func runHandoffScenario(t *testing.T, listen func(string) (net.Listener, error)) handoffResult {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cepm-ipc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("CEPM_HOME", dir) // restored by the test framework
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "run", "cepm.sock")

	handedOver := make(chan struct{})
	current, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	currentCtx, stopCurrent := context.WithCancel(context.Background())
	defer stopCurrent()
	currentDone := make(chan struct{})
	go func() {
		defer close(currentDone)
		Serve(currentCtx, current, func(ctx context.Context, req Request) Response {
			if req.Cmd == CmdPing {
				// Hand the socket over as soon as the handshake is
				// answered; the reply is already on its way back.
				defer func() {
					stopCurrent()
					<-handedOver
				}()
				return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
			}
			return Response{OK: true, Results: []ReloadResult{{ID: "aaa", Status: StatusReloaded}}}
		})
	}()

	// The successor: a host from before the protocol field, which would
	// carry out anything it is handed.
	var mu sync.Mutex
	var oldHostSaw []string
	successorErr := make(chan error, 1)
	oldClosed := make(chan func(), 1)
	go func() {
		<-currentCtx.Done()
		old, err := listen(sock) // replaces the socket, like a new leader
		successorErr <- err
		if err != nil {
			close(handedOver)
			return
		}
		oldCtx, stopOld := context.WithCancel(context.Background())
		oldDone := make(chan struct{})
		go func() {
			defer close(oldDone)
			Serve(oldCtx, old, func(ctx context.Context, req Request) Response {
				mu.Lock()
				oldHostSaw = append(oldHostSaw, req.Cmd)
				mu.Unlock()
				return Response{OK: true, Results: []ReloadResult{{ID: "aaa", Status: StatusReloaded}}}
			})
		}()
		oldClosed <- func() { stopOld(); <-oldDone }
		close(handedOver)
	}()

	// Whether this errors or succeeds depends on timing; what must never
	// happen is the old host executing the command.
	_, _ = Reload(context.Background(), []string{"aaa"})
	<-handedOver
	err = <-successorErr
	select {
	case stop := <-oldClosed:
		stop()
	default:
	}
	<-currentDone

	mu.Lock()
	defer mu.Unlock()
	return handoffResult{oldHostSaw: append([]string(nil), oldHostSaw...), successorErr: err}
}

// The handshake and the command must reach the same host process. With two
// connections a host can quit in between and a follower from before the
// upgrade can win leadership and receive a command this CLI only ever
// verified against its predecessor — so the command must ride the same
// connection the ping was answered on.
func TestOperationsSurviveALeaderHandoffAfterTheHandshake(t *testing.T) {
	res := runHandoffScenario(t, Listen)
	if res.successorErr != nil {
		t.Fatalf("the successor host never took the socket, so this test proves nothing: %v", res.successorErr)
	}
	for _, cmd := range res.oldHostSaw {
		if cmd != CmdPing {
			t.Errorf("the pre-protocol host executed %q after the handoff (saw %v)", cmd, res.oldHostSaw)
		}
	}
}

// And the scenario must say so when the successor never listened: without
// that, "the old host received nothing" would be true for the wrong reason
// and the test above would pass even with the fix reverted.
func TestHandoffScenarioReportsASuccessorThatCannotListen(t *testing.T) {
	before := countIPCTempDirs(t)
	res := runHandoffScenario(t, func(string) (net.Listener, error) {
		return nil, errors.New("injected listen failure")
	})
	if res.successorErr == nil {
		t.Error("a successor that cannot listen must be reported")
	}
	if after := countIPCTempDirs(t); after > before {
		t.Errorf("the scenario leaked temp directories: %d → %d", before, after)
	}
}

func countIPCTempDirs(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob("/tmp/cepm-ipc*")
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

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

// The CLI must never give up while the host is still working to contract:
// its default patience has to exceed the host's own budget for the same
// request, whatever the batch size.
func TestClientDeadlineExceedsTheHostBudget(t *testing.T) {
	for _, n := range []int{0, 1, 7, 20, 100} {
		ids := make([]string, n)
		req := Request{Cmd: CmdReload, IDs: ids}
		client, host := ClientDeadline(req), ReloadBudget(n)
		if client <= host {
			t.Errorf("reload of %d: client waits %v, host may take %v", n, client, host)
		}
	}
	if got, want := ClientDeadline(Request{Cmd: CmdUninstall}), UninstallBudget; got <= want {
		t.Errorf("uninstall: client waits %v, host may take %v", got, want)
	}
	for _, cmd := range []string{CmdPing, CmdListChrome} {
		got := ClientDeadline(Request{Cmd: cmd})
		if got <= RequestBudget {
			t.Errorf("%s: client waits %v, host may take %v", cmd, got, RequestBudget)
		}
		if got > 2*time.Minute {
			t.Errorf("%s should not wait unbounded, got %v", cmd, got)
		}
	}
}

// A caller that set its own deadline knows something this package does not,
// so the default must not extend it. Proved against a server that never
// answers: the call has to return by the caller's deadline, not the
// command's much longer default.
func TestCallerDeadlineIsNotOverridden(t *testing.T) {
	block := make(chan struct{})
	startTestServer(t, func(ctx context.Context, req Request) Response {
		if req.Cmd == CmdPing {
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
		<-block // never answers the operation
		return Response{OK: true}
	})
	// Registered after the server, so it runs before the stop that waits
	// for in-flight handlers: otherwise the two wait for each other.
	t.Cleanup(func() { close(block) })

	ids := make([]string, 20) // default would be ReloadBudget(20)+slack ≈ 85s
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Reload(ctx, ids); err == nil {
		t.Fatal("the call should have failed at the caller's deadline")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the caller's deadline was ignored: took %v", elapsed)
	}
}

// The server must give a command the same budget the client waits for and
// the host works to. It used to cut every connection at a flat five
// minutes, which a reload of ~100 extensions outgrows.
func TestServerDeadlineFollowsTheRequest(t *testing.T) {
	seen := make(chan Request, 4)
	orig := serverDeadlineFor
	serverDeadlineFor = func(req Request) time.Duration {
		seen <- req
		return orig(req)
	}
	stop := startTestServer(t, func(ctx context.Context, req Request) Response {
		if req.Cmd == CmdPing {
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
		return Response{OK: true, Results: []ReloadResult{}}
	})
	ids := make([]string, 100)
	if _, err := Reload(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	// Restore only once the server that reads it has stopped.
	stop()
	serverDeadlineFor = orig

	var reload *Request
	for len(seen) > 0 {
		req := <-seen
		if req.Cmd == CmdReload {
			r := req
			reload = &r
		}
	}
	if reload == nil {
		t.Fatal("the server never computed a deadline for the reload")
	}
	if len(reload.IDs) != 100 {
		t.Fatalf("the deadline was computed from %d ids, not the request's 100", len(reload.IDs))
	}
	// The invariant that matters: longer than the host's own budget for
	// the same work — which a fixed five minutes is not.
	if got, host := orig(*reload), ReloadBudget(100); got <= host {
		t.Errorf("server allows %v, host may take %v", got, host)
	}
	if ReloadBudget(100) <= 5*time.Minute {
		t.Skip("the old fixed 5m would still have sufficed for 100; raise the case")
	}
}
