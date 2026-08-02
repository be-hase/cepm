package nmhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/semver"

	"github.com/be-hase/cepm/internal/config"
	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/logx"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
)

const (
	pingInterval = 25 * time.Second // keeps the helper's service worker alive
	// requestTimeout is the budget for a request with no per-item work;
	// reloads add perExtensionTimeout for each extension so that a large
	// installation does not time out while the helper is still working.
	requestTimeout      = 10 * time.Second
	perExtensionTimeout = 3 * time.Second
	sendTimeout         = 5 * time.Second
	shutdownGrace       = 5 * time.Second
	// uninstallTimeout is long because the helper waits for the user to
	// answer Chrome's confirmation dialog.
	uninstallTimeout = 2 * time.Minute
	// minHelperVersion is the oldest helper this host can talk to; bump it
	// together with protocol changes.
	minHelperVersion = "0.1.0"
)

// Host is one native messaging host process. Chrome starts one per
// connectNative call; only the process holding the leader lock runs the
// socket server and the periodic updater.
type Host struct {
	version string
	log     *slog.Logger
	// cancel ends the host; safego calls it when an essential goroutine
	// dies of a panic, so the process exits visibly instead of lingering
	// half-alive (writer gone, socket dead) with no symptom but silence.
	cancel context.CancelFunc

	in  io.Reader
	out chan []byte // frames to Chrome, serialized by the writer goroutine

	mu      sync.Mutex
	pending map[string]chan json.RawMessage
	reqSeq  atomic.Int64

	startedAt       time.Time
	leader          atomic.Bool
	helperVersion   atomic.Value // string
	lastPong        atomic.Int64 // unix nano
	helperRefreshed atomic.Bool  // helper file refresh attempted (once per process)
	caughtUp        atomic.Bool  // startup catch-up reload done (once per process)
	// helperCompat gates every Chrome operation. Three-valued on purpose:
	// before a hello is processed the helper is *unknown*, and unknown must
	// not be treated as compatible — a request sent then could reach a
	// helper the host has no verdict on. Values are the compatVerdict
	// constants; the refreshed files fix too_old on the next Chrome start.
	helperCompat atomic.Int32

	// afterReloadAuthorized runs between authorizing a socket reload and
	// acting on it. Test-only: it is the seam a test needs to land a state
	// change exactly in that window, which otherwise only a sleep could
	// approximate.
	afterReloadAuthorized func()

	// schedulerWait and runUpdate are test seams (nil in production): the
	// first lets a test drive scheduler ticks without real time passing,
	// the second records what a cycle would have updated with. Set before
	// the scheduler starts; never mutated after.
	schedulerWait func(time.Duration) <-chan time.Time
	runUpdate     func(context.Context, *config.Config)

	// pendingReload holds ids whose reload is owed to Chrome: the update
	// already checked out their new code, but the reload has not been
	// confirmed. The set survives failed attempts and is retried on every
	// scheduler wake — without it, one transient helper error would leave
	// the user running old code until the next commit or Chrome restart.
	// The value is the generation the debt was recorded in: a flush may
	// only settle the generation it snapshotted, so a success from an old
	// attempt cannot erase a debt added while that attempt was in flight.
	pendingReloadMu  sync.Mutex
	pendingReload    map[string]uint64
	pendingReloadGen uint64
}

// compatVerdict values for Host.helperCompat.
const (
	compatUnknown int32 = iota
	compatOK
	compatTooOld
)

// safego runs fn on its own goroutine, logging a panic to the host log
// before deciding what dies with it. Chrome discards the host's stderr, so
// an unrecovered panic in any goroutine is an undiagnosable crash loop —
// exactly what the file logger exists to prevent; every goroutine this
// package spawns goes through here. An essential goroutine (writer, reader,
// leader duties) is one the host cannot serve without: losing it shuts the
// host down cleanly so Chrome can start a fresh one, instead of leaving a
// process that answers nothing.
func (h *Host) safego(name string, essential bool, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.log.Error("panic", "in", name, "recover", fmt.Sprint(r), "stack", string(debug.Stack()))
				if essential && h.cancel != nil {
					h.cancel()
				}
			}
		}()
		fn()
	}()
}

// addPendingReloads records ids whose reload is owed.
func (h *Host) addPendingReloads(ids []string) {
	h.pendingReloadMu.Lock()
	defer h.pendingReloadMu.Unlock()
	if h.pendingReload == nil {
		h.pendingReload = map[string]uint64{}
	}
	h.pendingReloadGen++
	for _, id := range ids {
		h.pendingReload[id] = h.pendingReloadGen
	}
}

// flushPendingReloads tries to deliver every owed reload. Only an outcome
// that settles the intent — reloaded, not installed, skipped by the user's
// choice — clears an id; transport errors, per-id errors and missing answers
// keep it owed for the next attempt.
func (h *Host) flushPendingReloads(ctx context.Context) {
	h.pendingReloadMu.Lock()
	snapshot := make(map[string]uint64, len(h.pendingReload))
	ids := make([]string, 0, len(h.pendingReload))
	for id, gen := range h.pendingReload {
		snapshot[id] = gen
		ids = append(ids, id)
	}
	h.pendingReloadMu.Unlock()
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)

	// A settle may only clear the generation this flush snapshotted: a new
	// debt for the same id, added while the attempt was in flight, is a
	// promise this attempt's result says nothing about.
	settleSnapshotted := func(settled []string) {
		h.pendingReloadMu.Lock()
		defer h.pendingReloadMu.Unlock()
		for _, id := range settled {
			if h.pendingReload[id] == snapshot[id] {
				delete(h.pendingReload, id)
			}
		}
	}

	// The debt was recorded at update time; the user may have disabled or
	// uninstalled since, so what is still owed is decided under the lock,
	// together with the send. When the state cannot be read nothing is sent
	// — and nothing is forgotten either.
	results, unwanted, err := h.reloadEnabled(ctx, ids)
	if len(unwanted) > 0 {
		h.log.Info("dropping owed reloads no longer wanted", "ids", unwanted)
		settleSnapshotted(unwanted)
	}
	if err != nil {
		h.log.Error("reload failed; keeping it for the next tick", "err", err, "ids", ids)
		return
	}
	var settled []string
	for _, r := range results {
		h.log.Info("reload", "id", r.ID, "status", r.Status, "err", r.Error)
		switch r.Status {
		case ipc.StatusReloaded, ipc.StatusNotInstalled, ipc.StatusSkippedDisabled,
			ipc.StatusSkippedSelf, ipc.StatusSkippedNotUnpacked:
			settled = append(settled, r.ID)
		}
	}
	settleSnapshotted(settled)
	h.pendingReloadMu.Lock()
	remaining := len(h.pendingReload)
	h.pendingReloadMu.Unlock()
	if remaining > 0 {
		h.log.Warn("reloads still owed; retrying on the next wake", "count", remaining)
	}
}

// CheckOrigin verifies the caller identified itself as the cepm helper.
// Chrome enforces allowed_origins itself, so this guards the case where the
// manifest was tampered with or the binary was started by something else.
func CheckOrigin(origin string) error {
	want := "chrome-extension://" + helperext.ExtensionID() + "/"
	if strings.TrimSuffix(origin, "/")+"/" != want {
		return fmt.Errorf("refusing to serve unknown extension %q (expected the cepm helper %s)",
			origin, helperext.ExtensionID())
	}
	return nil
}

// Run drives a host process until Chrome closes the port (stdin EOF).
func Run(ctx context.Context, version string) error {
	return RunIO(ctx, version, os.Stdin, os.Stdout)
}

// RunIO is Run with an injectable transport (tests substitute pipes for the
// stdio pair Chrome owns in production).
func RunIO(ctx context.Context, version string, in io.Reader, outW io.Writer) error {
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	logPath, err := paths.HostLogFile()
	if err != nil {
		return err
	}
	log, closeLog, err := logx.FileLogger(logPath, false)
	if err != nil {
		return err
	}
	defer closeLog()
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic", "recover", fmt.Sprint(r))
		}
	}()

	h := &Host{
		version:   version,
		log:       log,
		in:        in,
		out:       make(chan []byte, 16),
		pending:   map[string]chan json.RawMessage{},
		startedAt: time.Now(),
	}
	log.Info("host started", "pid", os.Getpid(), "version", version, "args", os.Args)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.cancel = cancel

	var wg sync.WaitGroup
	wg.Add(1)
	h.safego("writer", true, func() { // the only goroutine touching stdout
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-h.out:
				if err := WriteMessage(outW, frame); err != nil {
					log.Error("write to chrome failed", "err", err)
					cancel()
					return
				}
			}
		}
	})
	wg.Add(1)
	h.safego("pinger", true, func() { // keeps the MV3 service worker awake
		defer wg.Done()
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		var seq int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				seq++
				if err := h.send(ctx, pingMsg{Type: typePing, Seq: seq}); err != nil && ctx.Err() == nil {
					h.log.Warn("keep-alive ping not sent", "err", err)
				}
			}
		}
	})
	wg.Add(1)
	h.safego("leader duties", true, func() { // periodic updates + CLI socket, one process at a time
		defer wg.Done()
		h.runLeaderDuties(ctx)
	})

	// The reader blocks in a read that cannot be canceled, so it runs on its
	// own goroutine: if the writer dies (Chrome closed stdout) we must exit
	// rather than linger as a process that holds nothing and answers nothing.
	readErr := make(chan error, 1)
	h.safego("reader", true, func() { readErr <- h.readLoop(ctx) })

	select {
	case err = <-readErr: // stdin EOF: Chrome is gone
		cancel()
	case <-ctx.Done(): // writer failed, or the caller canceled
		err = ctx.Err()
	}
	// Bound the wait: a git subprocess in the periodic update can outlive
	// its context, and Chrome kills hosts that do not exit promptly.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		log.Warn("shutdown timed out; exiting anyway")
	}
	log.Info("host exiting", "err", err)
	if err == io.EOF || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (h *Host) readLoop(ctx context.Context) error {
	for {
		frame, err := ReadMessage(h.in)
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal(frame, &env); err != nil {
			h.log.Warn("undecodable message from helper", "err", err)
			continue
		}
		switch env.Type {
		case typeHello:
			var msg helloMsg
			if err := json.Unmarshal(frame, &msg); err == nil {
				h.helperVersion.Store(msg.HelperVersion)
				// Enforce, not just declare: an unparsable version is
				// treated as too old — fail closed, the refreshed files
				// repair it on the next Chrome start either way.
				tooOld := !semver.IsValid("v"+msg.HelperVersion) ||
					semver.Compare("v"+msg.HelperVersion, "v"+minHelperVersion) < 0
				if tooOld {
					h.helperCompat.Store(compatTooOld)
					h.log.Error("helper is older than this host supports; Chrome operations are paused",
						"helperVersion", msg.HelperVersion, "minHelperVersion", minHelperVersion,
						"fix", "restart Chrome to load the refreshed helper")
				} else {
					h.helperCompat.Store(compatOK)
					h.log.Info("helper connected", "helperVersion", msg.HelperVersion)
				}
			}
			// Everything here is off the read loop: sending can block for
			// seconds if the writer is wedged, and the follow-ups round-trip
			// through this very loop. This host process is the
			// freshly-installed binary (Chrome launches whatever the launcher
			// resolves), so it may carry newer helper files than the ones
			// Chrome just loaded.
			h.safego("hello follow-up", false, func() {
				ack := helloAckMsg{Type: typeHelloAck, HostVersion: h.version, MinHelperVersion: minHelperVersion}
				if err := h.send(ctx, ack); err != nil {
					h.log.Error("hello ack not sent", "err", err)
					return
				}
				h.maybeRefreshHelper(ctx)
				h.catchUpReload(ctx)
			})
		case typePong:
			h.lastPong.Store(time.Now().UnixNano())
		case typeReloadResult, typeListResult, typeUninstallResult:
			h.mu.Lock()
			ch := h.pending[env.RequestID]
			delete(h.pending, env.RequestID)
			h.mu.Unlock()
			if ch != nil {
				ch <- json.RawMessage(frame)
			}
		default:
			h.log.Warn("unknown message type from helper", "type", env.Type)
		}
	}
}

// send queues a message for the writer goroutine. It reports failure instead
// of dropping silently, so a caller waiting for a reply can fail fast rather
// than wait out its whole timeout for an answer to a message never sent.
func (h *Host) send(ctx context.Context, msg any) error {
	frame, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal outgoing message: %w", err)
	}
	select {
	case h.out <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(sendTimeout):
		return fmt.Errorf("cannot send to Chrome: the connection is stalled")
	}
}

// requestCtx sends a message carrying requestID and waits for the helper's
// reply until ctx expires; callers bound the wait with a timeout.
func (h *Host) requestCtx(ctx context.Context, requestID string, msg any) (json.RawMessage, error) {
	ch := make(chan json.RawMessage, 1)
	h.mu.Lock()
	h.pending[requestID] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, requestID)
		h.mu.Unlock()
	}()
	if err := h.send(ctx, msg); err != nil {
		return nil, err
	}
	select {
	case raw := <-ch:
		return raw, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("helper extension did not answer (is it loaded and enabled?): %w", ctx.Err())
	}
}

// request is requestCtx with the default short timeout.
func (h *Host) request(ctx context.Context, requestID string, msg any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return h.requestCtx(ctx, requestID, msg)
}

func (h *Host) nextRequestID() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), h.reqSeq.Add(1))
}

// helperGate refuses to relay Chrome operations to a helper this host has
// no compatible hello from: an incompatible one may misinterpret the
// request, and before any hello there is nobody verified to talk to — a
// clear refusal with the fix named beats acting wrongly or timing out in
// silence. Refused work stays owed (the debt machinery keeps it), so a
// pre-hello refusal costs one recheck, not the reload.
func (h *Host) helperGate() error {
	switch h.helperCompat.Load() {
	case compatOK:
		return nil
	case compatTooOld:
		hv, _ := h.helperVersion.Load().(string)
		return fmt.Errorf("the connected cepm helper (v%s) is older than this host supports (v%s); restart Chrome to load the refreshed helper",
			hv, minHelperVersion)
	default:
		return fmt.Errorf("the cepm helper has not connected yet; wait a moment, or check it is loaded and enabled in chrome://extensions")
	}
}

// Reload asks the helper to reload the given extension IDs. The deadline
// grows with the batch size so that reloading many extensions is not
// misreported as an unresponsive helper.
func (h *Host) Reload(ctx context.Context, ids []string) ([]ipc.ReloadResult, error) {
	if err := h.helperGate(); err != nil {
		return nil, err
	}
	id := h.nextRequestID()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout+time.Duration(len(ids))*perExtensionTimeout)
	defer cancel()
	raw, err := h.requestCtx(ctx, id, reloadReq{Type: typeReload, RequestID: id, ExtensionIDs: ids})
	if err != nil {
		return nil, err
	}
	var msg reloadResultMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	return msg.Results, nil
}

// ListChrome asks the helper for Chrome's unpacked extensions.
func (h *Host) ListChrome(ctx context.Context) ([]ipc.ChromeExt, error) {
	if err := h.helperGate(); err != nil {
		return nil, err
	}
	id := h.nextRequestID()
	raw, err := h.request(ctx, id, listReq{Type: typeList, RequestID: id})
	if err != nil {
		return nil, err
	}
	var msg listResultMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	if msg.Error != "" {
		// Never let "could not look" masquerade as "nothing is loaded":
		// callers act destructively on an empty list.
		return nil, fmt.Errorf("helper could not list extensions: %s", msg.Error)
	}
	return msg.Extensions, nil
}

// Uninstall asks the helper to uninstall an extension; Chrome shows a native
// confirmation dialog, so this can never remove anything silently. Waiting is
// bounded by the user's decision, hence the longer timeout.
func (h *Host) Uninstall(ctx context.Context, extID string) (status string, err error) {
	if err := h.helperGate(); err != nil {
		return "", err
	}
	id := h.nextRequestID()
	ctx, cancel := context.WithTimeout(ctx, uninstallTimeout)
	defer cancel()
	raw, err := h.requestCtx(ctx, id, uninstallReq{Type: typeUninstall, RequestID: id, ExtensionID: extID})
	if err != nil {
		return "", err
	}
	var msg uninstallResultMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "", err
	}
	if msg.Status == "error" {
		return msg.Status, fmt.Errorf("uninstall failed: %s", msg.Error)
	}
	return msg.Status, nil
}

// maybeRefreshHelper brings ~/.cepm/helper up to date with the helper files
// embedded in this binary, so a cepm upgrade propagates to the helper
// extension with no user action.
//
// It deliberately does not ask the helper to reload itself: the only way an
// extension can reload itself is chrome.runtime.reload(), which tears down
// this native messaging port, and Chrome does not reliably restart the
// helper's service worker afterwards — that would strand the connection
// until the next Chrome start. The rewritten files take effect on the next
// Chrome start anyway, which is exactly when the old code would stop running.
func (h *Host) maybeRefreshHelper(ctx context.Context) {
	if h.helperRefreshed.Load() {
		return
	}
	dir, err := paths.HelperDir()
	if err != nil {
		return
	}
	installed := helperext.InstalledVersion(dir)
	// Content, not just the marker: a corrupted file next to a current
	// marker must be repaired, not trusted.
	if installed == helperext.Version && helperext.InstalledMatches(dir) {
		h.helperRefreshed.Store(true)
		return
	}
	if err := helperext.Install(dir); err != nil {
		// Retried on the next connect rather than skipped for the session.
		h.log.Error("refresh helper files failed", "err", err)
		return
	}
	h.helperRefreshed.Store(true)
	h.log.Info("helper files refreshed; new version applies on the next Chrome start",
		"from", installed, "to", helperext.Version)
}

// The helper holds Chrome's management permission for *every* installed
// extension, so the host relays only ids the state accounts for. This bounds
// what a bug — a stale id list, a confused caller, a malformed request — can
// reach: it is not a security boundary against the user themselves, who owns
// the state file (see the trust note in internal/state). The sets differ by
// command because their meanings do: a reload only makes sense for a live,
// enabled extension, while a removal is also how cleanup clears the stale
// and orphan records left behind. Exported so tests hold their fake host to
// the same rules.

// enabledIDs is the set a reload may target: live and enabled right now.
func enabledIDs(st *state.State) map[string]bool {
	ok := map[string]bool{}
	for _, name := range st.RepoNames() {
		for _, e := range st.Repos[name].Extensions {
			if e.Enabled() {
				ok[e.ID] = true
			}
		}
	}
	return ok
}

// AuthorizeReload permits ids that are live and enabled right now.
func AuthorizeReload(ids []string) ([]string, error) {
	return authorize(ids, enabledIDs)
}

// reloadEnabled reloads the wanted ids while holding the update lock, and
// reports which of them the state no longer wants reloaded. Deciding from
// the state and then sending must be one step: otherwise a cepm disable can
// complete in between, and Chrome is told to reload an extension the user
// just turned off. Every reload path goes through here, so "disabled" and
// "reloaded" can only happen in one order or the other, never half of each.
//
// The lock is the same one every state writer takes. Nothing that already
// holds it may call this — which is why removals (cleanup sends those while
// holding the lock) are authorized without it.
func (h *Host) reloadEnabled(ctx context.Context, want []string) (results []ipc.ReloadResult, unwanted []string, err error) {
	err = updater.WithLock(ctx, func() error {
		st, lerr := state.LoadValid()
		if lerr != nil {
			return lerr
		}
		enabled := enabledIDs(st)
		var send []string
		for _, id := range want {
			if enabled[id] {
				send = append(send, id)
			} else {
				unwanted = append(unwanted, id)
			}
		}
		if len(send) == 0 {
			return nil
		}
		sort.Strings(send)
		results, lerr = h.Reload(ctx, send)
		return lerr
	})
	return results, unwanted, err
}

// AuthorizeRemoval permits live extensions plus validated stale/orphan
// records.
func AuthorizeRemoval(ids []string) ([]string, error) {
	return authorize(ids, func(st *state.State) map[string]bool {
		ok := map[string]bool{}
		for _, name := range st.RepoNames() {
			r := st.Repos[name]
			for _, e := range r.Extensions {
				ok[e.ID] = true
			}
			for _, s := range r.Stale {
				ok[s.ID] = true
			}
		}
		for _, o := range st.Orphans {
			ok[o.ID] = true
		}
		return ok
	})
}

func authorize(ids []string, allowed func(*state.State) map[string]bool) ([]string, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("no extension ids given")
	}
	// An invalid state cannot say who owns an id, so nothing is relayed.
	st, err := state.LoadValid()
	if err != nil {
		return nil, err
	}
	ok := allowed(st)
	for _, id := range ids {
		if !ok[id] {
			return nil, fmt.Errorf("extension %s is not managed by cepm", id)
		}
	}
	return ids, nil
}

// catchUpReload reloads every enabled extension once, right after Chrome
// connects. Updates pulled while Chrome was closed are on disk but Chrome may
// still run the code it cached in the previous session (service worker
// scripts in particular survive a restart), so without this the user would
// silently keep running stale code until the next update.
func (h *Host) catchUpReload(ctx context.Context) {
	// Claim-then-release rather than check-then-act: two hellos land close
	// together whenever the service worker restarts, and both passing a
	// plain Load check would reload every enabled extension twice.
	if !h.caughtUp.CompareAndSwap(false, true) {
		return
	}
	done := false
	defer func() {
		if !done {
			// A later hello (the service worker restarts often) retries,
			// rather than silently running stale code all session.
			h.caughtUp.Store(false)
		}
	}()
	st, err := state.LoadValid()
	if err != nil {
		h.log.Error("catch-up reload: load state", "err", err)
		return
	}
	var ids []string
	for _, name := range st.RepoNames() {
		for _, e := range st.Repos[name].Extensions {
			if e.Enabled() {
				ids = append(ids, e.ID)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	// Owed like any other reload, not fired once: flushPendingReloads
	// settles an id only on a final answer and keeps transport errors,
	// missing answers and per-id failures for the next scheduler tick.
	// Before this, one individually failed catch-up reload was written off
	// on transport success and the extension ran stale code all session.
	done = true // registered: from here the debt set owns the retries
	h.addPendingReloads(ids)
	h.flushPendingReloads(ctx)
	h.log.Info("catch-up reload flushed", "extensions", len(ids))
}

// runLeaderDuties keeps trying to become the leader; once it wins the lock it
// serves the CLI socket and runs periodic updates until ctx ends. The lock is
// held for the rest of the process lifetime (released on exit), so followers
// simply retry until the previous host dies.
func (h *Host) runLeaderDuties(ctx context.Context) {
	lockPath, err := paths.HostLockPath()
	if err != nil {
		h.log.Error("resolve lock path", "err", err)
		return
	}
	release, err := acquireLeadership(ctx, lockPath)
	if err != nil {
		if ctx.Err() == nil {
			h.log.Error("leader election failed", "err", err)
		}
		return
	}
	defer release()
	h.leader.Store(true)
	h.log.Info("became leader")

	sock, err := paths.SocketPath()
	if err != nil {
		h.log.Error("resolve socket path", "err", err)
		return
	}
	// Retry: giving up here would leave a host that talks to Chrome fine but
	// is invisible to the CLI for the rest of the session.
	var l net.Listener
	for attempt := 1; ; attempt++ {
		if l, err = ipc.Listen(sock); err == nil {
			break
		}
		h.log.Error("bind control socket", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	defer l.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	h.safego("ipc server", true, func() {
		defer wg.Done()
		ipc.Serve(ctx, l, h.handleIPC)
	})
	h.runScheduler(ctx)
	wg.Wait()
}

func (h *Host) handleIPC(ctx context.Context, req ipc.Request) (resp ipc.Response) {
	// A request-scoped panic (these paths act on state and Chrome — the
	// likeliest place for one) answers the one CLI call with an error
	// instead of killing the host for everyone.
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("panic", "in", "handleIPC", "cmd", req.Cmd, "recover", fmt.Sprint(r), "stack", string(debug.Stack()))
			resp = ipc.Response{Error: "internal error in the host; see " + logHint()}
		}
	}()
	// A CLI from a different protocol generation must not be half-served:
	// pings still answer (that is how doctor diagnoses the mismatch), but
	// everything that acts is refused with the fix named.
	if req.Cmd != ipc.CmdPing && req.Protocol != ipc.ProtocolVersion {
		return ipc.Response{Error: fmt.Sprintf(
			"this cepm CLI (protocol %d) does not match the running host (protocol %d); restart Chrome to relaunch the updated host",
			req.Protocol, ipc.ProtocolVersion)}
	}
	switch req.Cmd {
	case ipc.CmdPing:
		compat := ipc.HelperCompatUnknown
		switch h.helperCompat.Load() {
		case compatOK:
			compat = ipc.HelperCompatOK
		case compatTooOld:
			compat = ipc.HelperCompatTooOld
		}
		info := &ipc.HostInfo{
			Version:          h.version,
			PID:              os.Getpid(),
			Leader:           h.leader.Load(),
			StartedAt:        h.startedAt,
			MinHelperVersion: minHelperVersion,
			HelperCompat:     compat,
			Protocol:         ipc.ProtocolVersion,
		}
		if hv, ok := h.helperVersion.Load().(string); ok {
			info.HelperVersion = hv
		}
		if ns := h.lastPong.Load(); ns > 0 {
			info.LastPong = time.Unix(0, ns)
		}
		return ipc.Response{OK: true, Host: info}
	case ipc.CmdReload:
		if _, err := AuthorizeReload(req.IDs); err != nil {
			return ipc.Response{Error: err.Error()}
		}
		if h.afterReloadAuthorized != nil {
			h.afterReloadAuthorized()
		}
		// Authorized again inside the lock, which is what actually decides:
		// the check above only fails fast with a clear message.
		results, unwanted, err := h.reloadEnabled(ctx, req.IDs)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		for _, id := range unwanted {
			// Not StatusSkippedDisabled: that one means Chrome has the
			// extension switched off. Here it is cepm's own state that
			// changed under the request.
			results = append(results, ipc.ReloadResult{ID: id, Status: ipc.StatusSkippedStateChanged})
		}
		return ipc.Response{OK: true, Results: results}
	case ipc.CmdListChrome:
		exts, err := h.ListChrome(ctx)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		return ipc.Response{OK: true, Extensions: exts}
	case ipc.CmdUninstall:
		if _, err := AuthorizeRemoval([]string{req.ID}); err != nil {
			return ipc.Response{Error: err.Error()}
		}
		status, err := h.Uninstall(ctx, req.ID)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		return ipc.Response{OK: true, Status: status}
	default:
		return ipc.Response{Error: fmt.Sprintf("unknown command %q", req.Cmd)}
	}
}

// logHint names the host log file for error messages: the person reading
// them is stuck and needs the path, not a description of it.
func logHint() string {
	if p, err := paths.HostLogFile(); err == nil {
		return p
	}
	return "the host log"
}

// runScheduler performs periodic pulls while Chrome is running. The first run
// happens shortly after startup; later runs are spaced by the configured
// interval with ±10% jitter so a fleet of machines doesn't hit the git server
// in lockstep.
// configRecheckInterval bounds how often the scheduler re-reads a config
// that is currently unusable (does not parse, or has auto update off): the
// user is likely editing the file, and before this the host had to be
// restarted to notice.
const configRecheckInterval = time.Minute

func (h *Host) runScheduler(ctx context.Context) {
	wait := h.schedulerWait
	if wait == nil {
		wait = func(d time.Duration) <-chan time.Time { return time.After(d) }
	}
	run := h.runUpdate
	if run == nil {
		run = func(ctx context.Context, cfg *config.Config) { h.autoUpdate(ctx, cfg) }
	}

	delay := time.Minute + rand.N(time.Minute)
	// E2E tests shorten the first run to avoid multi-minute waits.
	if v := os.Getenv("CEPM_BOOTSTRAP_UPDATE_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			delay = d
		}
	}
	h.log.Info("auto update scheduled", "firstRunIn", delay)

	// The config is re-read every cycle, so edits (a fixed typo, auto
	// toggled, a new interval or stash_dirty) apply without a Chrome
	// restart. standing dedupes the log: a paused scheduler must not repeat
	// the same complaint every recheck, but must say when it resumes.
	standing := ""
	pause := func(reason string, log func()) {
		if standing != reason {
			log()
			standing = reason
		}
		delay = configRecheckInterval
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-wait(delay):
		}
		// Owed reloads are independent of the config: a broken file or
		// auto=false pauses *updates*, not the debt the helper still owes
		// from catch-up or earlier ticks.
		h.flushPendingReloads(ctx)
		cfg, err := config.Load()
		if err != nil {
			pause("err:"+err.Error(), func() {
				h.log.Error("load config failed; periodic updates paused until it parses",
					"err", err, "recheckIn", configRecheckInterval)
			})
			continue
		}
		if !cfg.Update.Auto {
			pause("disabled", func() {
				h.log.Info("auto update disabled by config; re-checking periodically",
					"recheckIn", configRecheckInterval)
			})
			continue
		}
		if standing != "" {
			h.log.Info("config usable again; auto update resumed", "interval", cfg.Update.Interval)
			standing = ""
		}
		run(ctx, cfg)
		jitter := time.Duration(float64(cfg.Update.Interval) * 0.1)
		delay = cfg.Update.Interval - jitter + rand.N(2*jitter)
	}
}

func (h *Host) autoUpdate(ctx context.Context, cfg *config.Config) {
	h.log.Info("auto update started")
	results, err := updater.Update(ctx, nil, updater.Options{StashDirty: cfg.Git.StashDirty})
	if err != nil {
		h.log.Error("auto update failed", "err", err)
		// Reloads owed from earlier ticks do not depend on this update.
		h.flushPendingReloads(ctx)
		return
	}
	var ids []string
	for _, r := range results {
		switch {
		case r.Err != nil:
			h.log.Warn("repo update failed", "repo", r.Name, "err", r.Err)
		case r.Skipped:
			h.log.Info("repo skipped", "repo", r.Name, "reason", r.SkipReason)
		case r.Updated:
			h.log.Info("repo updated", "repo", r.Name, "from", r.OldRef, "to", r.NewRef,
				"changed", len(r.Changed), "added", len(r.Added))
			for _, c := range r.Changed {
				ids = append(ids, c.ID)
			}
		}
	}
	h.addPendingReloads(ids)
	// Owed reloads from earlier ticks are retried even when this update
	// found nothing new.
	h.flushPendingReloads(ctx)
}
