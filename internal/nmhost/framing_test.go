package nmhost

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	msgs := []string{`{"type":"ping","seq":1}`, `{}`, strings.Repeat("x", 100000)}
	for _, m := range msgs {
		if err := WriteMessage(&buf, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range msgs {
		got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(want))
		}
	}
	if _, err := ReadMessage(&buf); err != io.EOF {
		t.Errorf("expected io.EOF on empty stream, got %v", err)
	}
}

func TestWriteMessageRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, make([]byte, maxOutgoing+1)); err == nil {
		t.Error("expected error for message above Chrome's 1MB limit")
	}
}

func TestReadMessageTruncated(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	truncated := bytes.NewReader(buf.Bytes()[:buf.Len()-3])
	if _, err := ReadMessage(truncated); err == nil {
		t.Error("expected error for truncated frame")
	}
}
