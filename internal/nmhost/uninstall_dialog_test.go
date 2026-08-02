package nmhost

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/state"
)

// fakeAliveTimeout replaces the "the helper has gone quiet" clock with one
// the test drives: quiet fires only when the test says so, and every reset
// is observable. Nothing here waits on real time.
func fakeAliveTimeout(t *testing.T) (quiet chan time.Time, resets chan struct{}) {
	t.Helper()
	quiet = make(chan time.Time, 1)
	resets = make(chan struct{}, 16)
	prev := aliveTimeout
	// Registered before the host exists so it runs after the host's own
	// cleanup: restoring a global while the read loop still runs is a race.
	t.Cleanup(func() { aliveTimeout = prev })
	aliveTimeout = func(time.Duration) (<-chan time.Time, func(), func()) {
		return quiet, func() { resets <- struct{}{} }, func() {}
	}
	return quiet, resets
}

// Chrome's uninstall dialog is a person deciding, and nothing closes it when
// a deadline expires. The caller holds the update lock across the whole call
// precisely so the extension id cannot be registered again while the dialog
// is open — so the wait has to follow the dialog, not a clock. Every
// heartbeat from the helper starts the patience over.
func TestUninstallWaitsWhileTheDialogIsStillOpen(t *testing.T) {
	_, resets := fakeAliveTimeout(t)
	seedHostState(t, state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA})
	helper := &fakeHelper{
		holdUninstall: make(chan struct{}),
		beatUninstall: make(chan struct{}),
		uninstallSeen: make(chan struct{}, 1),
	}
	h, _ := newTestHost(t, helper)

	type result struct {
		status string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := h.Uninstall(context.Background(), idA)
		done <- result{status, err}
	}()

	<-helper.uninstallSeen
	// Two rounds of "the dialog is still up", each one observed as a
	// restarted deadline on the host side. The timer bounds the *failure*
	// only — a heartbeat that never restarts the wait would otherwise hang
	// here until the whole package times out — and is never waited on when
	// the code is right.
	for i := range 2 {
		helper.beatUninstall <- struct{}{}
		lost := time.NewTimer(30 * time.Second)
		select {
		case <-resets:
		case r := <-done:
			t.Fatalf("gave up on beat %d while the dialog was still open: %+v", i, r)
		case <-lost.C:
			t.Fatalf("beat %d never restarted the wait: a dialog still on screen would "+
				"outlive the update lock", i)
		}
		lost.Stop()
	}

	close(helper.holdUninstall) // the user finally presses Remove
	r := <-done
	if r.err != nil {
		t.Fatalf("uninstall after a long dialog: %v", r.err)
	}
	if r.status != "uninstalled" {
		t.Errorf("status = %q, want %q", r.status, "uninstalled")
	}
}

// Waiting on liveness must not become waiting forever: when the helper stops
// answering its worker is gone, and the pending uninstall goes with it.
func TestUninstallGivesUpWhenTheHelperGoesQuiet(t *testing.T) {
	quiet, _ := fakeAliveTimeout(t)
	seedHostState(t, state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA})
	helper := &fakeHelper{
		holdUninstall: make(chan struct{}),
		beatUninstall: make(chan struct{}),
		uninstallSeen: make(chan struct{}, 1),
	}
	h, _ := newTestHost(t, helper)

	errs := make(chan error, 1)
	go func() {
		_, err := h.Uninstall(context.Background(), idA)
		errs <- err
	}()

	<-helper.uninstallSeen
	quiet <- time.Time{}

	err := <-errs
	if err == nil {
		t.Fatal("a helper that stopped answering must end the wait")
	}
	if !strings.Contains(err.Error(), "stopped responding") {
		t.Errorf("the error should say the helper went quiet, got: %v", err)
	}
	close(helper.holdUninstall) // let the fake helper finish
}
