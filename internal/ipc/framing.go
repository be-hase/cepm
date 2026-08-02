package ipc

import (
	"bufio"
	"errors"
	"fmt"
)

// errTooLong marks the size refusal so the server can answer with it instead
// of dropping the connection without a word.
var errTooLong = errors.New("message too long")

// MaxMessage bounds one newline-delimited request or response.
//
// Both sides read until a newline, so a peer that never sends one would
// otherwise make the other grow a buffer until the process dies. The socket
// directory is 0700, so this is not a boundary against another user — it is
// the same reason the native messaging framing has a limit: a malformed or
// runaway sender must not be able to take the host down with it.
//
// Generous by design. The largest real message is a reload naming every
// extension a user has, which is a few hundred bytes each.
const MaxMessage = 1 << 20 // 1 MiB

// readLine reads one message, refusing anything longer than MaxMessage
// instead of buffering it.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxMessage {
			return nil, fmt.Errorf("%w: over the %d byte limit", errTooLong, MaxMessage)
		}
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			continue // a long line, not the end of one
		}
		if err != nil {
			return nil, err
		}
		return buf, nil
	}
}
