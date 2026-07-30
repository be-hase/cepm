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

func serveConn(ctx context.Context, conn net.Conn, h Handler) {
	defer conn.Close()
	// Generous: an uninstall request waits for the user to answer Chrome's
	// confirmation dialog.
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req Request
	var resp Response
	if err := json.Unmarshal(line, &req); err != nil {
		resp = Response{Error: "invalid request: " + err.Error()}
	} else {
		resp = h(ctx, req)
	}
	enc, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(enc, '\n'))
}
