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
	conn, r, err := dialHost(ctx, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return exchange(conn, r, req, patienceFor(ctx, req))
}

// patienceFor is how long silence from the host is tolerated. A caller's own
// deadline wins: it knows something this does not.
func patienceFor(ctx context.Context, req Request) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return ClientDeadline(req)
}

// dialHost opens a connection to the host with the deadline ctx implies, or
// the one this command's contract implies when the caller set none.
func dialHost(ctx context.Context, req Request) (net.Conn, *bufio.Reader, error) {
	cmd := req.Cmd
	sock, err := paths.SocketPath()
	if err != nil {
		return nil, nil, err
	}
	slog.Debug("host request", "cmd", cmd, "socket", sock)
	conn, err := dialWithRetry(ctx, sock)
	if err != nil {
		slog.Debug("host not reachable", "socket", sock, "err", err)
		return nil, nil, err
	}
	// A caller's own deadline wins: it knows something this does not.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(ClientDeadline(req)))
	}
	return conn, bufio.NewReader(conn), nil
}

// exchange sends one request and reads its response on an open connection,
// skipping the host's interim "still working" lines. patience bounds each
// wait between lines rather than the whole exchange: an uninstall waits on a
// Chrome dialog that only a person can close, and giving up on it would
// release the update lock with that dialog still on screen.
func exchange(conn net.Conn, r *bufio.Reader, req Request, patience time.Duration) (*Response, error) {
	req.Protocol = ProtocolVersion
	enc, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(enc, '\n')); err != nil {
		return nil, fmt.Errorf("write to host: %w", err)
	}
	var resp Response
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read from host: %w", err)
		}
		resp = Response{}
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("parse host response: %w", err)
		}
		if !resp.Working {
			break
		}
		// The host is still on it; start the patience over.
		_ = conn.SetDeadline(time.Now().Add(patience))
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

// ErrProtocolMismatch means the running host is a different cepm generation
// than this binary. It is checked before every operation, never assumed away.
var ErrProtocolMismatch = errors.New("the running cepm host is a different version than this cepm; restart Chrome to relaunch the updated host")

// call is Call with a protocol handshake first, for every command that
// acts. The gate has to be on this side: a host old enough to predate the
// protocol field ignores it as unknown JSON and would carry out whatever it
// is sent, so the request must not be sent at all. Ping is the one command
// exempt — it is how the mismatch is discovered, here and in doctor.
//
// Handshake and operation share one connection, deliberately: with two, a
// host can quit between them and a follower from before the upgrade can win
// leadership, bind the socket, and receive an operation this CLI already
// "verified" against its predecessor. One connection means the peer that
// answered the ping is the peer that gets the command — or the connection
// dies with it and nothing is executed.
func call(ctx context.Context, req Request) (*Response, error) {
	conn, r, err := dialHost(ctx, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	resp, err := exchange(conn, r, Request{Cmd: CmdPing}, patienceFor(ctx, req))
	if err != nil {
		return nil, err
	}
	if resp.Host == nil || resp.Host.Protocol != ProtocolVersion {
		return nil, ErrProtocolMismatch
	}
	return exchange(conn, r, req, patienceFor(ctx, req))
}

// Reload asks the host to reload the given extension IDs via the helper.
func Reload(ctx context.Context, ids []string) ([]ReloadResult, error) {
	resp, err := call(ctx, Request{Cmd: CmdReload, IDs: ids})
	if err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// ListChrome returns the unpacked extensions Chrome currently has loaded.
func ListChrome(ctx context.Context) ([]ChromeExt, error) {
	resp, err := call(ctx, Request{Cmd: CmdListChrome})
	if err != nil {
		return nil, err
	}
	return resp.Extensions, nil
}

// Uninstall asks Chrome (via the helper) to uninstall an extension. Chrome
// shows a native confirmation dialog; the returned status reflects the
// user's choice. The wait is not bounded by how long someone takes to
// decide — the host reports progress and this keeps waiting — so callers
// should leave ctx without a deadline unless they mean to abandon a dialog
// that is still open.
func Uninstall(ctx context.Context, id string) (string, error) {
	resp, err := call(ctx, Request{Cmd: CmdUninstall, ID: id})
	if err != nil {
		return "", err
	}
	return resp.Status, nil
}
