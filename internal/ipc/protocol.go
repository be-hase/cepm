// Package ipc implements the local control channel between the cepm CLI and
// the native host process: newline-delimited JSON over a Unix socket in
// ~/.cepm/run (mode 0700), one request per connection.
package ipc

import "time"

// Commands.
const (
	CmdPing       = "ping"
	CmdReload     = "reload"
	CmdListChrome = "listChrome"
	CmdUninstall  = "uninstall"
)

// Result statuses (mirroring the helper extension's reports).
const (
	StatusReloaded     = "reloaded"
	StatusNotInstalled = "not_installed"
	StatusError        = "error"
	StatusUninstalled  = "uninstalled"
	StatusCancelled    = "cancelled"
)

type Request struct {
	Cmd string   `json:"cmd"`
	IDs []string `json:"ids,omitempty"` // extension IDs for CmdReload
	ID  string   `json:"id,omitempty"`  // extension ID for CmdUninstall
}

type Response struct {
	OK         bool           `json:"ok"`
	Error      string         `json:"error,omitempty"`
	Host       *HostInfo      `json:"host,omitempty"`
	Results    []ReloadResult `json:"results,omitempty"`
	Extensions []ChromeExt    `json:"extensions,omitempty"`
	Status     string         `json:"status,omitempty"` // for CmdUninstall
}

// HostInfo describes the running native host (ping response).
type HostInfo struct {
	Version       string    `json:"version"`
	PID           int       `json:"pid"`
	Leader        bool      `json:"leader"`
	StartedAt     time.Time `json:"startedAt"`
	LastPong      time.Time `json:"lastPong"` // last keep-alive reply from the helper
	HelperVersion string    `json:"helperVersion,omitempty"`
}

type ReloadResult struct {
	ID     string `json:"id"`
	Status string `json:"status"` // reloaded | not_installed | error
	Error  string `json:"error,omitempty"`
}

// ChromeExt is an unpacked extension as reported by Chrome's management API.
type ChromeExt struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
}
