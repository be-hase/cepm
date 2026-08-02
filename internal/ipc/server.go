package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// Handler processes one CLI request inside the native host.
type Handler func(ctx context.Context, req Request) Response

// Listen binds the Unix socket, removing a stale socket file first. The
// caller owns the listener; closing it removes the socket.
func Listen(sock string) (net.Listener, error) {
	if _, err := os.Stat(sock); err == nil {
		// Either a stale file from a crashed host or a live listener that is
		// about to go away (we only get here after winning the leader lock).
		_ = os.Remove(sock)
	}
	return net.Listen("unix", sock)
}

// Serve accepts connections until the listener closes or ctx is canceled,
// then waits for in-flight requests so a reply is not cut off mid-write.
func Serve(ctx context.Context, l net.Listener, h Handler) {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	var backoff time.Duration
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			// A persistent error (fd exhaustion) would otherwise spin this
			// loop at full speed for the life of the process.
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff < time.Second {
				backoff *= 2
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveConn(ctx, conn, h)
		}()
	}
}

// serverDeadlineFor is how long a connection may stay open for one command,
// a variable so a test can observe that the deadline really is per-request
// rather than a constant the budget can outgrow.
var serverDeadlineFor = ClientDeadline

// keepaliveTick paces the interim "still working" lines. Test seam: a test
// fires the returned channel itself, so proving the behaviour never depends
// on real time passing.
var keepaliveTick = func() (c <-chan time.Time, stop func()) {
	t := time.NewTicker(keepaliveEvery)
	return t.C, t.Stop
}

// keepaliveEvery is well inside every command's budget, so a client sees
// several signs of life before its own patience could run out.
const keepaliveEvery = 5 * time.Second

// runReporting runs the handler, telling the client it is still working for
// as long as it takes.
//
// Some commands wait on a person: "uninstall" is a Chrome confirmation
// dialog, and nothing closes that dialog when a deadline expires. A client
// that gave up would release the update lock it holds precisely to keep the
// extension id from being registered again while the dialog is open. So the
// client's patience must track the host's progress rather than a guess about
// how long someone takes to decide.
func runReporting(ctx context.Context, conn net.Conn, req Request, h Handler) Response {
	done := make(chan Response, 1)
	go func() { done <- h(ctx, req) }()
	tick, stop := keepaliveTick()
	defer stop()
	for {
		select {
		case resp := <-done:
			return resp
		case <-tick:
			_ = conn.SetDeadline(time.Now().Add(serverDeadlineFor(req)))
			if _, err := conn.Write(append(mustMarshal(Response{Working: true}), '\n')); err != nil {
				// The client is gone. Let the handler finish — it is
				// acting on Chrome and must not be abandoned half-done —
				// and discard what it returns.
				return <-done
			}
		}
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Response is a fixed struct of plain types; this cannot fail.
		panic(err)
	}
	return b
}

func serveConn(ctx context.Context, conn net.Conn, h Handler) {
	defer conn.Close()
	// A floor for reading the request itself; extended per command below,
	// because a large reload is entitled to more than any fixed number.
	_ = conn.SetDeadline(time.Now().Add(RequestBudget + clientSlack))
	r := bufio.NewReader(conn)
	// A connection carries at most a protocol handshake (a ping) and then
	// one command: the client needs both to reach the same host process, or
	// a leader handoff in between could route the command to a host it
	// never verified. Anything after a non-ping command ends the loop.
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req Request
		var resp Response
		if err := json.Unmarshal(line, &req); err != nil {
			resp = Response{Error: "invalid request: " + err.Error()}
		} else {
			// The same budget the client waits for and the host works to:
			// a fixed five minutes cut off a large reload the host was
			// still entitled to be running.
			_ = conn.SetDeadline(time.Now().Add(serverDeadlineFor(req)))
			resp = runReporting(ctx, conn, req, h)
		}
		enc, err := json.Marshal(resp)
		if err != nil {
			return
		}
		if _, err := conn.Write(append(enc, '\n')); err != nil {
			return
		}
		if req.Cmd != CmdPing {
			return
		}
	}
}
