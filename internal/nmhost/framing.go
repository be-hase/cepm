// Package nmhost implements the native messaging host process that Chrome
// launches for the cepm helper extension.
package nmhost

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Chrome's native messaging framing is a 4-byte native-endian length prefix
// followed by UTF-8 JSON. Chrome accepts up to 1 MB per message from the
// host and sends up to 64 MB to it.
const (
	maxIncoming = 64 << 20
	maxOutgoing = 1 << 20
)

// ReadMessage reads one framed message. It returns io.EOF when the stream is
// closed (Chrome quit or disconnected the port).
func ReadMessage(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		// io.ReadFull already reports the two cases apart: io.EOF for a
		// close between frames (Chrome quit — the clean shutdown), and
		// ErrUnexpectedEOF for a stream cut inside the prefix, which is a
		// real failure and must not be mistaken for a clean exit.
		return nil, err
	}
	n := binary.NativeEndian.Uint32(lenBuf[:])
	if n > maxIncoming {
		return nil, fmt.Errorf("incoming message of %d bytes exceeds limit", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// WriteMessage writes one framed message.
func WriteMessage(w io.Writer, msg []byte) error {
	if len(msg) > maxOutgoing {
		return fmt.Errorf("outgoing message of %d bytes exceeds Chrome's 1MB limit", len(msg))
	}
	var lenBuf [4]byte
	binary.NativeEndian.PutUint32(lenBuf[:], uint32(len(msg)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}
