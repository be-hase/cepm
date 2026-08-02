package ipc

import (
	"context"
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
	// No caller deadline — that is the case this is about. A deadline of
	// the caller's own bounds the whole operation on purpose, progress or
	// not; what has to survive progress is the *silence* budget.
	const patience = 300 * time.Millisecond
	prevPatience := clientPatience
	t.Cleanup(func() { clientPatience = prevPatience })
	clientPatience = func(Request) time.Duration { return patience }

	done := make(chan result, 1)
	go func() {
		status, err := Uninstall(context.Background(), "abcdefghijklmnopabcdefghijklmnop")
		done <- result{status, err}
	}()

	<-reached
	// The handler is deliberately held past the client's whole patience.
	// This is the one thing here that cannot be proven without real time
	// passing: the claim is about a deadline. Each interim line has to push
	// that deadline out, so the total wait below (5×) exceeds it several
	// times over while no single gap comes close.
	for i := range 5 {
		select {
		case tick <- time.Time{}: // "still working"
		case r := <-done:
			t.Fatalf("gave up after %d progress line(s), with the host still working: %+v", i, r)
		}
		time.Sleep(patience / 3)
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
