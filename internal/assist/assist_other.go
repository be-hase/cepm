//go:build !darwin && !linux

package assist

import "errors"

var errUnsupported = errors.New("not supported on this platform")

func CopyToClipboard(s string) error { return errUnsupported }
func OpenExtensionsPage() error      { return errUnsupported }
