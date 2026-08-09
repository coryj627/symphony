//go:build windows

package web

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isIPv6Unavailable(err error) bool {
	return errors.Is(err, windows.WSAEAFNOSUPPORT) ||
		errors.Is(err, windows.WSAEPROTONOSUPPORT) ||
		errors.Is(err, windows.WSAEADDRNOTAVAIL)
}
