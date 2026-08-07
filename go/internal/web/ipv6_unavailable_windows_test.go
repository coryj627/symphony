//go:build windows

package web

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

type opaqueWrappedError struct {
	err error
}

func (e opaqueWrappedError) Error() string { return "localized socket failure" }
func (e opaqueWrappedError) Unwrap() error { return e.err }

func TestIPv6UnavailableClassifierWindows(t *testing.T) {
	for _, errno := range []error{windows.WSAEAFNOSUPPORT, windows.WSAEPROTONOSUPPORT, windows.WSAEADDRNOTAVAIL} {
		err := opaqueWrappedError{err: fmt.Errorf("listen: %w", errno)}
		if !isIPv6Unavailable(err) {
			t.Errorf("supported IPv6-unavailable errno was rejected")
		}
	}
	if isIPv6Unavailable(opaqueWrappedError{err: windows.WSAEADDRINUSE}) {
		t.Error("unexpected bind failure was treated as IPv6 absence")
	}
}
