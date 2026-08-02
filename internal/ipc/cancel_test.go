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

// Two things move the read deadline — a progress line pushes it out, a
// cancelled caller pulls it into the past — and the last SetDeadline wins.
// Checking ctx before extending only narrows that window; the ordering has
// to be settled where both changes are made, or a progress line arriving
// alongside a Ctrl-C puts the read back to sleep for a caller who has left.
func TestARestartCannotUndoACancellation(t *testing.T) {
	restarts := make(chan struct{}, 4)
	prev := restartPatience
	t.Cleanup(func() { restartPatience = prev })
	restartPatience = func(net.Conn, time.Duration) { restarts <- struct{}{} }

	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	d := &deadline{conn: a}

	d.restart(time.Hour)
	if len(restarts) != 1 {
		t.Fatalf("fixture: a restart before any cancellation should happen, got %d", len(restarts))
	}
	<-restarts

	d.cancel()
	d.restart(time.Hour) // the progress line that lost the race
	if len(restarts) != 0 {
		t.Error("a cancelled read must not be given another budget of patience")
	}
}

// The sequential test above proves the flag is consulted; it does not prove
// the two are ordered against each other. They run on different goroutines
// in production — the watcher cancels while exchange restarts — so dropping
// the mutex would put the read back to sleep for a caller who has left.
// Under -race, which CI runs, that regression shows up here as a data race
// on the flag rather than as a rare hang in the field.
func TestCancelAndRestartAreOrderedAgainstEachOther(t *testing.T) {
	prev := restartPatience
	t.Cleanup(func() { restartPatience = prev })
	restartPatience = func(net.Conn, time.Duration) {}

	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })

	for range 50 {
		d := &deadline{conn: a}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; d.cancel() }()
		go func() { defer wg.Done(); <-start; d.restart(time.Hour) }()
		close(start)
		wg.Wait()
		if !d.cancelled {
			t.Fatal("a cancellation must never be forgotten")
		}
	}
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
