package nmhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/be-hase/cepm/internal/config"
	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/logx"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/updater"
)

const (
	pingInterval   = 25 * time.Second // keeps the helper's service worker alive
	requestTimeout = 10 * time.Second
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
				h.send(pingMsg{Type: typePing, Seq: seq})
			}
		}
	}()
	wg.Add(1)
	go func() { // leadership: periodic updates + CLI socket, one process at a time
		defer wg.Done()
		h.runLeaderDuties(ctx)
	}()

	// Reader loop on the main goroutine; EOF means Chrome is gone.
	err = h.readLoop(ctx)
	cancel()
	wg.Wait()
	log.Info("host exiting", "err", err)
	if err == io.EOF {
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
			h.send(helloAckMsg{Type: typeHelloAck, HostVersion: h.version, MinHelperVersion: minHelperVersion})
			// This host process is the freshly-installed binary (Chrome
			// launches whatever the launcher resolves), so it may carry newer
			// helper files than the ones Chrome just loaded. Runs in a
			// goroutine: it round-trips through this read loop.
			go h.maybeRefreshHelper(ctx)
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

func (h *Host) send(msg any) {
	frame, err := json.Marshal(msg)
	if err != nil {
		h.log.Error("marshal outgoing message", "err", err)
		return
	}
	select {
	case h.out <- frame:
	case <-time.After(5 * time.Second):
		h.log.Error("writer stalled; dropping message")
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
	h.send(msg)
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

// Reload asks the helper to reload the given extension IDs.
func (h *Host) Reload(ctx context.Context, ids []string) ([]ipc.ReloadResult, error) {
	id := h.nextRequestID()
	raw, err := h.request(ctx, id, reloadReq{Type: typeReload, RequestID: id, ExtensionIDs: ids})
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
// embedded in this binary and asks the running helper to reload itself, so a
// cepm upgrade propagates to the helper extension with zero user action. The
// reload only happens after the version marker was written successfully,
// which is what terminates the refresh → reconnect → refresh cycle.
func (h *Host) maybeRefreshHelper(ctx context.Context) {
	if !h.helperRefreshed.CompareAndSwap(false, true) {
		return
	}
	dir, err := paths.HelperDir()
	if err != nil {
		return
	}
	installed := helperext.InstalledVersion(dir)
	if installed == helperext.Version {
		return
	}
	if err := helperext.Install(dir); err != nil {
		h.log.Error("refresh helper files failed", "err", err)
		return
	}
	h.log.Info("helper files refreshed", "from", installed, "to", helperext.Version)
	results, err := h.Reload(ctx, []string{helperext.ExtensionID()})
	if err != nil {
		h.log.Warn("helper self-reload not confirmed (it may reload on next Chrome start)", "err", err)
		return
	}
	for _, r := range results {
		h.log.Info("helper self-reload", "status", r.Status, "err", r.Error)
	}
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
	l, err := ipc.Listen(sock)
	if err != nil {
		h.log.Error("bind control socket", "err", err)
		return
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
		results, err := h.Reload(ctx, req.IDs)
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
