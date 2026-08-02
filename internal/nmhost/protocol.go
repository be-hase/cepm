package nmhost

import "github.com/be-hase/cepm/internal/ipc"

// Message types exchanged with the helper extension. Field names must match
// internal/helperext/assets/background.js.

const (
	typeHello           = "hello"
	typeHelloAck        = "helloAck"
	typePing            = "ping"
	typePong            = "pong"
	typeReload          = "reload"
	typeReloadResult    = "reloadResult"
	typeList            = "listExtensions"
	typeListResult      = "listResult"
	typeUninstall       = "uninstall"
	typeUninstallResult = "uninstallResult"
	// typeUninstallPending is the helper repeating "Chrome's confirmation
	// dialog is still open". It is what keeps the host waiting; see
	// Host.Uninstall.
	typeUninstallPending = "uninstallPending"
)

type envelope struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
}

type helloMsg struct {
	Type          string `json:"type"`
	HelperVersion string `json:"helperVersion"`
	// Protocol is what the running background.js was generated with; zero
	// means a helper from before it announced one.
	Protocol int `json:"protocol"`
}

type helloAckMsg struct {
	Type             string `json:"type"`
	HostVersion      string `json:"hostVersion"`
	MinHelperVersion string `json:"minHelperVersion"`
}

type pingMsg struct {
	Type string `json:"type"`
	Seq  int64  `json:"seq"`
}

type reloadReq struct {
	Type         string   `json:"type"`
	RequestID    string   `json:"requestId"`
	ExtensionIDs []string `json:"extensionIds"`
}

type reloadResultMsg struct {
	Type      string             `json:"type"`
	RequestID string             `json:"requestId"`
	Results   []ipc.ReloadResult `json:"results"`
}

type listReq struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
}

type listResultMsg struct {
	Type       string          `json:"type"`
	RequestID  string          `json:"requestId"`
	Extensions []ipc.ChromeExt `json:"extensions"`
	Error      string          `json:"error,omitempty"`
}

type uninstallReq struct {
	Type        string `json:"type"`
	RequestID   string `json:"requestId"`
	ExtensionID string `json:"extensionId"`
}

type uninstallResultMsg struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Status    string `json:"status"` // uninstalled | cancelled | not_installed | error
	Error     string `json:"error,omitempty"`
}
