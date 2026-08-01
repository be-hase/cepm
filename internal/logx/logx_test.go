package logx

import (
	"os"
	"path/filepath"
	"testing"
)

// The log names repositories, extensions and errors — owner-only, like
// everything else under ~/.cepm.
func TestFileLoggerIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.log")
	logger, closeFn, err := FileLogger(path, false)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello")
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("log mode = %o, want no group/other access", perm)
	}
}
