//go:build !darwin && !linux

package assist

import "errors"

var errUnsupported = errors.New("not supported on this platform")

func copyToClipboard(s string) error { return errUnsupported }
func openExtensionsPage() error      { return errUnsupported }
