// Package logx sets up slog logging for cepm processes.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

const maxLogSize = 5 * 1024 * 1024

// FileLogger returns a logger writing to path, rotating the file to
// "<path>.old" once it exceeds maxLogSize. The returned closer must be called
// on shutdown.
func FileLogger(path string, verbose bool) (*slog.Logger, func() error, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		_ = os.Rename(path, path+".old")
	}
	// 0600: the log names repositories, extensions and errors — owner-only
	// like everything else under ~/.cepm.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}
	return newLogger(f, verbose), f.Close, nil
}

// StderrLogger returns a logger for interactive CLI use.
func StderrLogger(verbose bool) *slog.Logger {
	return newLogger(os.Stderr, verbose)
}

func newLogger(w io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
