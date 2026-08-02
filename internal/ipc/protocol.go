// Package ipc implements the local control channel between the cepm CLI and
// the native host process: newline-delimited JSON over a Unix socket in
// ~/.cepm/run (mode 0700), one request per connection.
package ipc

import "time"

// ProtocolVersion is the CLI⇄host protocol generation, independent of the
// release version: the binary on disk (the CLI) and the host Chrome launched
// at session start can be different builds, and a mismatched pair must fail
// with "restart Chrome" instead of misunderstanding each other. Bump it
// whenever the request/response shapes change meaning.
const ProtocolVersion = 1

// The host's budget for a request and the CLI's patience for the answer are
// one contract seen from two sides, so it lives here — the package both
// import — instead of being restated in each and drifting apart. It did:
// the CLI gave up after a flat 30s while the host was entitled to 10s plus
// 3s per extension, so a reload of seven extensions could time out on the
// CLI while the host was still working to contract.
const (
	// RequestBudget is the host's allowance for a request with no per-item
	// work; reloads add PerExtensionBudget per extension so a large
	// installation is not mistaken for an unresponsive helper.
	RequestBudget      = 10 * time.Second
	PerExtensionBudget = 3 * time.Second
	// UninstallBudget is long because the helper waits for the user to
	// answer Chrome's confirmation dialog.
	UninstallBudget = 2 * time.Minute
	// clientSlack is what the CLI allows itself on top of the host's
	// budget: the protocol handshake, the dial retry, and transport.
	clientSlack = 15 * time.Second
)

// ReloadBudget is how long the host may spend reloading n extensions.
func ReloadBudget(n int) time.Duration {
	return RequestBudget + time.Duration(n)*PerExtensionBudget
}

// ClientDeadline is how long the CLI waits for req's answer when its caller
// set no deadline of its own. Always longer than the host's own budget for
// the same request, so a host working to contract is never cut off.
func ClientDeadline(req Request) time.Duration {
	switch req.Cmd {
	case CmdReload:
		return ReloadBudget(len(req.IDs)) + clientSlack
	case CmdUninstall:
		return UninstallBudget + clientSlack
	default:
		return RequestBudget + clientSlack
	}
}

// Commands.
const (
	CmdPing       = "ping"
	CmdReload     = "reload"
	CmdListChrome = "listChrome"
	CmdUninstall  = "uninstall"
)

// Result statuses (mirroring the helper extension's reports).
const (
	StatusReloaded = "reloaded"
	// StatusSkippedDisabled: the user turned the extension off in Chrome, so
	// the helper leaves it that way instead of re-enabling it on every update.
	StatusSkippedDisabled = "skipped_disabled"
	StatusSkippedSelf     = "skipped_self"
	// StatusSkippedNotUnpacked: the id belongs to a Web Store install (a repo
	// pinning the "key" of a published extension); the helper refuses to
	// touch anything the user did not load unpacked. Final: retrying cannot
	// change it.
	StatusSkippedNotUnpacked = "skipped_not_unpacked"
	// StatusSkippedStateChanged: cepm stopped wanting this reload between
	// accepting the request and acting on it — the extension was disabled or
	// its repository uninstalled in the meantime. It says nothing about
	// Chrome, which may well still be running the extension.
	StatusSkippedStateChanged = "skipped_state_changed"
	StatusNotInstalled        = "not_installed"
	StatusError               = "error"
	StatusUninstalled         = "uninstalled"
	StatusCancelled           = "cancelled"
)

type Request struct {
	Cmd string   `json:"cmd"`
	IDs []string `json:"ids,omitempty"` // extension IDs for CmdReload
	ID  string   `json:"id,omitempty"`  // extension ID for CmdUninstall
	// Protocol is the sender's ProtocolVersion; zero means a CLI that
	// predates protocol reporting. The host answers pings regardless (so
	// doctor can diagnose the mismatch) but refuses everything else.
	Protocol int `json:"protocol,omitempty"`
}

type Response struct {
	OK         bool           `json:"ok"`
	Error      string         `json:"error,omitempty"`
	Host       *HostInfo      `json:"host,omitempty"`
	Results    []ReloadResult `json:"results,omitempty"`
	Extensions []ChromeExt    `json:"extensions,omitempty"`
	Status     string         `json:"status,omitempty"` // for CmdUninstall
}

// Helper compatibility verdicts, as reported in HostInfo.HelperCompat. A
// string, not a bool, deliberately: a HostInfo from a host that predates the
// field decodes to "", which readers must treat as "this host does not
// report compatibility" — a zero-valued bool would have read as a verdict.
const (
	HelperCompatUnknown = "unknown" // no hello processed yet
	HelperCompatOK      = "ok"
	HelperCompatTooOld  = "too_old"
)

// HostInfo describes the running native host (ping response).
type HostInfo struct {
	Version       string    `json:"version"`
	PID           int       `json:"pid"`
	Leader        bool      `json:"leader"`
	StartedAt     time.Time `json:"startedAt"`
	LastPong      time.Time `json:"lastPong"` // last keep-alive reply from the helper
	HelperVersion string    `json:"helperVersion,omitempty"`
	// MinHelperVersion is the oldest helper this host drives; when the
	// connected helper is older (or its version is unparsable), HelperCompat
	// is HelperCompatTooOld and the host refuses to relay Chrome operations.
	MinHelperVersion string `json:"minHelperVersion,omitempty"`
	HelperCompat     string `json:"helperCompat,omitempty"`
	// Protocol is the CLI⇄host protocol generation (see ProtocolVersion);
	// zero means the host predates protocol reporting.
	Protocol int `json:"protocol,omitempty"`
}

type ReloadResult struct {
	ID string `json:"id"`
	// Status is one of the Status* constants above: reloaded,
	// not_installed, skipped_disabled, skipped_self, skipped_state_changed,
	// or error.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ChromeExt is an unpacked extension as reported by Chrome's management API.
type ChromeExt struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
}
