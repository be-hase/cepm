package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"syscall"
	"time"

	"github.com/be-hase/cepm/internal/paths"
)

// ErrHostNotRunning means no native host is listening on the socket, i.e.
// Chrome is not running or the helper extension is not loaded/connected.
var ErrHostNotRunning = errors.New("cepm native host is not running (is Chrome running with the helper extension loaded?)")

// dialRetry covers the small gap while a new host process takes over
// leadership after a Chrome service-worker restart.
const dialRetry = 3 * time.Second

// Call sends one request to the native host and returns its response.
func Call(ctx context.Context, req Request) (*Response, error) {
	sock, err := paths.SocketPath()
	if err != nil {
		return nil, err
	}
	slog.Debug("host request", "cmd", req.Cmd, "ids", len(req.IDs), "socket", sock)
	conn, err := dialWithRetry(ctx, sock)
	if err != nil {
		slog.Debug("host not reachable", "socket", sock, "err", err)
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	enc, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(enc, '\n')); err != nil {
		return nil, fmt.Errorf("write to host: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read from host: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse host response: %w", err)
	}
	if !resp.OK {
		return &resp, fmt.Errorf("host error: %s", resp.Error)
	}
	return &resp, nil
}

func dialWithRetry(ctx context.Context, sock string) (net.Conn, error) {
	deadline := time.Now().Add(dialRetry)
	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", sock)
		if err == nil {
			return conn, nil
		}
		// Only the states a leader handoff passes through are worth the
		// retry: no socket yet (ENOENT) or a socket nobody accepts on
		// (ECONNREFUSED). A permission error or an over-long path will not
		// heal in three seconds, and reporting it as "is Chrome running?"
		// sends the user debugging the wrong thing.
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("dial host socket: %w", err)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("%w (last dial error: %v)", ErrHostNotRunning, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last dial error: %v)", ErrHostNotRunning, err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Ping checks host liveness.
func Ping(ctx context.Context) (*HostInfo, error) {
	resp, err := Call(ctx, Request{Cmd: CmdPing})
	if err != nil {
		return nil, err
	}
	return resp.Host, nil
}

// Reload asks the host to reload the given extension IDs via the helper.
func Reload(ctx context.Context, ids []string) ([]ReloadResult, error) {
	resp, err := Call(ctx, Request{Cmd: CmdReload, IDs: ids})
	if err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// ListChrome returns the unpacked extensions Chrome currently has loaded.
func ListChrome(ctx context.Context) ([]ChromeExt, error) {
	resp, err := Call(ctx, Request{Cmd: CmdListChrome})
	if err != nil {
		return nil, err
	}
	return resp.Extensions, nil
}

// Uninstall asks Chrome (via the helper) to uninstall an extension. Chrome
// shows a native confirmation dialog; the returned status reflects the
// user's choice. Callers should use a generous ctx deadline.
func Uninstall(ctx context.Context, id string) (string, error) {
	resp, err := Call(ctx, Request{Cmd: CmdUninstall, ID: id})
	if err != nil {
		return "", err
	}
	return resp.Status, nil
}
