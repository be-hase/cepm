package cli

import (
	"strings"
	"testing"
)

// Chrome always passes the caller's origin as argv[1]. Starting the host
// without one — or for an origin that is not the cepm helper — must fail:
// the origin check is what stops arbitrary local programs from driving the
// reload/uninstall relay.
func TestNativeHostRequiresTheHelperOrigin(t *testing.T) {
	startFakeHost(t)
	for _, args := range [][]string{
		{"native-host"},
		{"native-host", "not-an-origin"},
		{"native-host", "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/"},
	} {
		out, err := run(t, "", args...)
		if err == nil {
			t.Errorf("%v should refuse to start:\n%s", args, out)
		} else if strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%v failed for the wrong reason: %v", args, err)
		}
	}
}
