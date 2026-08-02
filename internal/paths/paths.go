// Package paths resolves cepm's own directory layout (~/.cepm) and
// Chrome-related locations on the host OS.
package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// CepmDir returns the cepm home directory, absolute and with symlinks
// resolved. It honors $CEPM_HOME (mainly for tests) and defaults to ~/.cepm.
//
// Resolving is not cosmetic. An unpacked extension's id is a hash of the
// absolute directory Chrome loaded it from, and Chrome hashes the path with
// symlinks already resolved. Hashing "~/.cepm/repos/..." while ~/.cepm is a
// symlink yields an id Chrome never assigned — and nothing downstream can
// catch it, because state.Validate re-derives the id the same wrong way and
// agrees with itself.
func CepmDir() (string, error) {
	dir := os.Getenv("CEPM_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		dir = filepath.Join(home, ".cepm")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", dir, err)
	}
	return Canonical(abs)
}

// Canonical resolves the symlinks in absPath, which need not exist yet: the
// leading part that does exist is resolved and the rest is appended. Failing
// on a missing path would make the id of a directory depend on whether it
// had been created when the id was computed — cepm derives ids for clones it
// is about to make, and for clones the user has deleted.
func Canonical(absPath string) (string, error) {
	clean := filepath.Clean(absPath)
	head, tail := clean, ""
	for {
		resolved, err := filepath.EvalSymlinks(head)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %q: %w", absPath, err)
		}
		parent := filepath.Dir(head)
		if parent == head {
			// Not one component of this path exists; there is nothing to
			// resolve and the cleaned path is the best answer available.
			return clean, nil
		}
		tail = filepath.Join(filepath.Base(head), tail)
		head = parent
	}
}

func sub(elem ...string) (string, error) {
	dir, err := CepmDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{dir}, elem...)...), nil
}

func ReposDir() (string, error)       { return sub("repos") }
func HelperDir() (string, error)      { return sub("helper") }
func BinDir() (string, error)         { return sub("bin") }
func LauncherPath() (string, error)   { return sub("bin", "cepm-host") }
func RunDir() (string, error)         { return sub("run") }
func LogsDir() (string, error)        { return sub("logs") }
func ConfigFile() (string, error)     { return sub("config.toml") }
func StateFile() (string, error)      { return sub("state.json") }
func SocketPath() (string, error)     { return sub("run", "cepm.sock") }
func HostLockPath() (string, error)   { return sub("run", "host.lock") }
func UpdateLockPath() (string, error) { return sub("run", "update.lock") }
func HostLogFile() (string, error)    { return sub("logs", "host.log") }

// EnsureLayout creates the ~/.cepm directory tree. run/ is 0700 because the
// Unix socket lives there and its directory permissions are the access control.
func EnsureLayout() error {
	// Everything is owner-only: the clones are internal extension source and
	// the logs describe them, and Chrome and the native host run as this same
	// user, so nothing needs to let anyone else in. A lax home directory
	// permission must not expose ~/.cepm.
	dirs := []struct {
		f    func() (string, error)
		perm os.FileMode
	}{
		{CepmDir, 0o700},
		{ReposDir, 0o700},
		{HelperDir, 0o700},
		{BinDir, 0o700},
		{RunDir, 0o700},
		{LogsDir, 0o700},
	}
	for _, d := range dirs {
		p, err := d.f()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(p, d.perm); err != nil {
			return err
		}
		// MkdirAll does not tighten permissions on pre-existing directories.
		if err := os.Chmod(p, d.perm); err != nil {
			return err
		}
	}
	return nil
}
