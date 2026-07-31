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
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // writer: the only goroutine touching stdout
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
	}()
	wg.Add(1)
	go func() { // pinger: keeps the MV3 service worker awake
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
	}()
	wg.Add(1)
	go func() { // leadership: periodic updates + CLI socket, one process at a time
		defer wg.Done()
		h.runLeaderDuties(ctx)
	}()

	// The reader blocks in a read that cannot be canceled, so it runs on its
	// own goroutine: if the writer dies (Chrome closed stdout) we must exit
	// rather than linger as a process that holds nothing and answers nothing.
	readErr := make(chan error, 1)
	go func() { readErr <- h.readLoop(ctx) }()

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
				h.log.Info("helper connected", "helperVersion", msg.HelperVersion)
			}
			// Everything here is off the read loop: sending can block for
			// seconds if the writer is wedged, and the follow-ups round-trip
			// through this very loop. This host process is the
			// freshly-installed binary (Chrome launches whatever the launcher
			// resolves), so it may carry newer helper files than the ones
			// Chrome just loaded.
			go func() {
				ack := helloAckMsg{Type: typeHelloAck, HostVersion: h.version, MinHelperVersion: minHelperVersion}
				if err := h.send(ctx, ack); err != nil {
					h.log.Error("hello ack not sent", "err", err)
					return
				}
				h.maybeRefreshHelper(ctx)
				h.catchUpReload(ctx)
			}()
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

// Reload asks the helper to reload the given extension IDs. The deadline
// grows with the batch size so that reloading many extensions is not
// misreported as an unresponsive helper.
func (h *Host) Reload(ctx context.Context, ids []string) ([]ipc.ReloadResult, error) {
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
	if installed == helperext.Version {
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

// managedIDs restricts what the control socket may act on to extensions cepm
// registered (plus stale records it is meant to clean up). The socket is only
// reachable by this user, but the helper holds the management permission for
// *every* installed extension, so the host must not relay arbitrary IDs.
func managedIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("no extension ids given")
	}
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, name := range st.RepoNames() {
		r := st.Repos[name]
		for _, e := range r.Extensions {
			known[e.ID] = true
		}
		for _, s := range r.Stale {
			known[s.ID] = true
		}
	}
	for _, o := range st.Orphans {
		known[o.ID] = true
	}
	for _, id := range ids {
		if !known[id] {
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
	if h.caughtUp.Load() {
		return
	}
	st, err := state.Load()
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
	results, err := h.Reload(ctx, ids)
	if err != nil {
		// Leave the flag unset: a later hello (the service worker restarts
		// often) retries, rather than silently running stale code all session.
		h.log.Warn("catch-up reload failed; will retry on the next helper connect", "err", err)
		return
	}
	h.caughtUp.Store(true)
	h.log.Info("catch-up reload done", "extensions", len(results))
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
	go func() {
		defer wg.Done()
		ipc.Serve(ctx, l, h.handleIPC)
	}()
	h.runScheduler(ctx)
	wg.Wait()
}

func (h *Host) handleIPC(ctx context.Context, req ipc.Request) ipc.Response {
	switch req.Cmd {
	case ipc.CmdPing:
		info := &ipc.HostInfo{
			Version:   h.version,
			PID:       os.Getpid(),
			Leader:    h.leader.Load(),
			StartedAt: h.startedAt,
		}
		if hv, ok := h.helperVersion.Load().(string); ok {
			info.HelperVersion = hv
		}
		if ns := h.lastPong.Load(); ns > 0 {
			info.LastPong = time.Unix(0, ns)
		}
		return ipc.Response{OK: true, Host: info}
	case ipc.CmdReload:
		ids, err := managedIDs(req.IDs)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		results, err := h.Reload(ctx, ids)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		return ipc.Response{OK: true, Results: results}
	case ipc.CmdListChrome:
		exts, err := h.ListChrome(ctx)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		return ipc.Response{OK: true, Extensions: exts}
	case ipc.CmdUninstall:
		if _, err := managedIDs([]string{req.ID}); err != nil {
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

// runScheduler performs periodic pulls while Chrome is running. The first run
// happens shortly after startup; later runs are spaced by the configured
// interval with ±10% jitter so a fleet of machines doesn't hit the git server
// in lockstep.
func (h *Host) runScheduler(ctx context.Context) {
	cfg, err := config.Load()
	if err != nil {
		h.log.Error("load config", "err", err)
		return
	}
	if !cfg.Update.Auto {
		h.log.Info("auto update disabled by config")
		<-ctx.Done()
		return
	}
	delay := time.Minute + rand.N(time.Minute)
	// E2E tests shorten the first run to avoid multi-minute waits.
	if v := os.Getenv("CEPM_BOOTSTRAP_UPDATE_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			delay = d
		}
	}
	h.log.Info("auto update scheduled", "interval", cfg.Update.Interval, "firstRunIn", delay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		h.autoUpdate(ctx, cfg)
		jitter := time.Duration(float64(cfg.Update.Interval) * 0.1)
		delay = cfg.Update.Interval - jitter + rand.N(2*jitter)
	}
}

func (h *Host) autoUpdate(ctx context.Context, cfg *config.Config) {
	h.log.Info("auto update started")
	results, err := updater.Update(ctx, nil, updater.Options{StashDirty: cfg.Git.StashDirty})
	if err != nil {
		h.log.Error("auto update failed", "err", err)
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
	if len(ids) == 0 {
		return
	}
	results2, err := h.Reload(ctx, ids)
	if err != nil {
		h.log.Error("reload after auto update failed", "err", err)
		return
	}
	for _, r := range results2 {
		h.log.Info("reload", "id", r.ID, "status", r.Status, "err", r.Error)
	}
}
