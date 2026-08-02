package ipc

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/paths"
)

// Cancelling reaches DialContext for free, but nothing after it: once the
// connection is up the read runs to its deadline. That deadline is now a
// silence budget measured in minutes for an uninstall — the host waits on a
// Chrome dialog — so a caller who gives up has to be able to actually leave.
// Ctrl-C would otherwise appear to do nothing.
func TestCancellingStopsAWaitAlreadyInProgress(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	stop := startTestServer(t, func(_ context.Context, req Request) Response {
		if req.Cmd == CmdPing {
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
		reached <- struct{}{}
		<-release
		return Response{OK: true, Status: StatusUninstalled}
	})
	t.Cleanup(stop)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := Uninstall(ctx, "abcdefghijklmnopabcdefghijklmnop")
		errs <- err
	}()

	<-reached
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled caller should be told just that, got: %v", err)
		}
	case <-time.After(30 * time.Second):
		// Bounds the failure only; the passing path never waits here.
		t.Fatal("cancelling did not interrupt the wait")
	}
	unblock()
}

// Both sides read until a newline. A peer that never sends one would
// otherwise make the other grow a buffer for as long as it keeps writing,
// which for the host means being killed by a runaway on its own socket.
func TestAnOversizedMessageIsRefusedRatherThanBuffered(t *testing.T) {
	stop := startTestServer(t, func(_ context.Context, req Request) Response {
		return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
	})
	t.Cleanup(stop)

	sock, err := paths.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	// No newline anywhere: the whole point is that the server must stop
	// accumulating rather than wait for one that is never coming.
	blob := strings.Repeat("x", 64*1024)
	var written int
	for written < MaxMessage+len(blob) {
		n, err := conn.Write([]byte(blob))
		written += n
		if err != nil {
			break // the server hung up on us, which is the refusal
		}
	}

	// The refusal has to come from the limit, not from the connection
	// deadline expiring much later: a server that buffers to the end of its
	// budget is the failure being ruled out. This bound is far above the
	// immediate answer and far below the request budget, so which one
	// happened is never ambiguous.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("no refusal arrived; the server was still accumulating: %v", err)
	}
	if !strings.Contains(string(line), "too long") {
		t.Errorf("the refusal should say what happened, got: %s", line)
	}
}

// The client reads until a newline too, so a host that goes wrong — or is
// not the host at all, on a socket path anything running as this user could
// have bound — must not be able to grow the CLI's buffer without end.
func TestAnOversizedResponseIsRefusedByTheClient(t *testing.T) {
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
	t.Cleanup(func() { l.Close() })

	// A "host" that answers every request with an endless line.
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				blob := []byte(strings.Repeat("x", 64*1024))
				for {
					if _, err := conn.Write(blob); err != nil {
						return
					}
				}
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Ping(ctx); err == nil {
		t.Fatal("an endless response line must be refused, not buffered")
	} else if !strings.Contains(err.Error(), "too long") {
		t.Errorf("the refusal should name the limit, got: %v", err)
	}
}
