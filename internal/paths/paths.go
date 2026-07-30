// Package paths resolves cepm's own directory layout (~/.cepm) and
// Chrome-related locations on the host OS.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// CepmDir returns the cepm home directory. It honors $CEPM_HOME (mainly for
// tests) and defaults to ~/.cepm.
func CepmDir() (string, error) {
	if dir := os.Getenv("CEPM_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".cepm"), nil
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
func CLILogFile() (string, error)     { return sub("logs", "cli.log") }

// EnsureLayout creates the ~/.cepm directory tree. run/ is 0700 because the
// Unix socket lives there and its directory permissions are the access control.
func EnsureLayout() error {
	dirs := []struct {
		f    func() (string, error)
		perm os.FileMode
	}{
		{CepmDir, 0o755},
		{ReposDir, 0o755},
		{HelperDir, 0o755},
		{BinDir, 0o755},
		{RunDir, 0o700},
		{LogsDir, 0o755},
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
