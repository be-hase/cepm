package ipc

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Some commands wait on a person. "uninstall" puts a Chrome confirmation
// dialog on screen, and a client that gives up on it releases the update
// lock it holds precisely to stop the extension id from being registered
// again while that dialog is still live. So the host reports that it is
// still working, and the client's patience starts over each time it does.
func TestTheClientKeepsWaitingWhileTheHostReportsProgress(t *testing.T) {
	// The tick is fired by the test, so nothing here depends on real time.
	tick := make(chan time.Time)
	prev := keepaliveTick
	t.Cleanup(func() { keepaliveTick = prev })
	keepaliveTick = func() (<-chan time.Time, func()) { return tick, func() {} }

	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	stop := startTestServer(t, func(ctx context.Context, req Request) Response {
		if req.Cmd == CmdPing {
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
		reached <- struct{}{}
		<-release
		return Response{OK: true, Status: StatusUninstalled}
	})
	t.Cleanup(stop)
	// Registered after the server, so it runs before the server is stopped:
	// a failing assertion would otherwise leave the handler blocked here
	// while cleanup waits for the server to finish serving it.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	type result struct {
		status string
		err    error
	}
	// Every restart of the patience is observed, so nothing here waits for a
	// deadline to expire — or sleeps to arrange one.
	restarts := make(chan time.Duration, 16)
	prevRestart := restartPatience
	t.Cleanup(func() { restartPatience = prevRestart })
	restartPatience = func(conn net.Conn, d time.Duration) {
		prevRestart(conn, d)
		restarts <- d
	}

	done := make(chan result, 1)
	go func() {
		// No caller deadline — that is the case this is about. A deadline of
		// the caller's own bounds the whole operation on purpose, progress
		// or not; what has to survive progress is the *silence* budget.
		status, err := Uninstall(context.Background(), "abcdefghijklmnopabcdefghijklmnop")
		done <- result{status, err}
	}()

	<-reached
	for i := range 5 {
		select {
		case tick <- time.Time{}: // "still working"
		case r := <-done:
			t.Fatalf("gave up after %d progress line(s), with the host still working: %+v", i, r)
		}
		select {
		case d := <-restarts:
			if d != ClientDeadline(Request{Cmd: CmdUninstall}) {
				t.Errorf("restart %d used %v, want the uninstall budget", i, d)
			}
		case r := <-done:
			t.Fatalf("gave up on progress line %d: %+v", i, r)
		case <-time.After(30 * time.Second):
			// Bounds the failure only; the passing path never waits here.
			t.Fatalf("progress line %d did not restart the patience", i)
		}
	}
	unblock()

	r := <-done
	if r.err != nil {
		t.Fatalf("the client gave up on a host that was still reporting progress: %v", r.err)
	}
	if r.status != StatusUninstalled {
		t.Errorf("status = %q, want %q", r.status, StatusUninstalled)
	}
}

// The interim lines are not answers: a client that mistook one for the
// response would report an empty result as success.
func TestProgressLinesAreNeverMistakenForTheAnswer(t *testing.T) {
	tick := make(chan time.Time)
	prev := keepaliveTick
	t.Cleanup(func() { keepaliveTick = prev })
	keepaliveTick = func() (<-chan time.Time, func()) { return tick, func() {} }

	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	stop := startTestServer(t, func(ctx context.Context, req Request) Response {
		if req.Cmd == CmdPing {
			return Response{OK: true, Host: &HostInfo{Protocol: ProtocolVersion}}
		}
		reached <- struct{}{}
		<-release
		return Response{OK: false, Error: "the user said no"}
	})
	t.Cleanup(stop)
	// See above: this has to run before the server is stopped.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	errs := make(chan error, 1)
	go func() {
		_, err := Uninstall(context.Background(), "abcdefghijklmnopabcdefghijklmnop")
		errs <- err
	}()

	<-reached
	tick <- time.Time{}
	unblock()

	err := <-errs
	if err == nil {
		t.Fatal("a progress line must not be returned as a successful answer")
	}
	if !strings.Contains(err.Error(), "the user said no") {
		t.Errorf("the real answer should reach the caller, got: %v", err)
	}
}
