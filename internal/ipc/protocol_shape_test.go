package ipc

import (
	"reflect"
	"strings"
	"testing"
)

// The wire shape and ProtocolVersion have to move together. Neither side can
// negotiate: the CLI on disk and the host Chrome launched at session start
// are different builds, and the handshake only compares this one number. A
// field added or removed without a bump means both sides claim to agree
// while one of them misreads the other — the "working" line, for instance,
// reads to an older CLI as a failed final answer, which releases the update
// lock with a Chrome dialog still on screen.
//
// So this pins the shape. Changing it fails here, and the fix is to decide
// whether the change alters meaning (bump ProtocolVersion) or not (update
// the list) — deliberately, rather than by omission.
func TestProtocolVersionIsPinnedToTheWireShape(t *testing.T) {
	const shapeAtVersion = 2
	if ProtocolVersion != shapeAtVersion {
		t.Fatalf("ProtocolVersion = %d but the shape below was last reviewed at %d; "+
			"check the change and update this test", ProtocolVersion, shapeAtVersion)
	}
	for _, tc := range []struct {
		what any
		want []string
	}{
		{Request{}, []string{"cmd", "ids", "id", "protocol"}},
		{Response{}, []string{"ok", "error", "host", "results", "extensions", "status", "working"}},
		{HostInfo{}, []string{"version", "pid", "leader", "startedAt", "lastPong", "helperVersion",
			"minHelperVersion", "helperCompat", "helperProtocol", "helperProtocolWanted", "protocol"}},
		{ReloadResult{}, []string{"id", "status", "error"}},
		{ChromeExt{}, []string{"id", "name", "version", "enabled"}},
	} {
		typ := reflect.TypeOf(tc.what)
		var got []string
		for i := range typ.NumField() {
			tag := typ.Field(i).Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			got = append(got, strings.Split(tag, ",")[0])
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s fields = %v, want %v", typ.Name(), got, tc.want)
		}
	}
}
