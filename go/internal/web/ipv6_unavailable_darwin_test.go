//go:build darwin

package web

import (
	"fmt"
	"syscall"
	"testing"
)

type opaqueWrappedError struct {
	err error
}

func (e opaqueWrappedError) Error() string { return "localized socket failure" }
func (e opaqueWrappedError) Unwrap() error { return e.err }

func TestIPv6UnavailableClassifierDarwin(t *testing.T) {
	for _, errno := range []error{syscall.EAFNOSUPPORT, syscall.EPROTONOSUPPORT, syscall.EADDRNOTAVAIL} {
		err := opaqueWrappedError{err: fmt.Errorf("listen: %w", errno)}
		if !isIPv6Unavailable(err) {
			t.Errorf("supported IPv6-unavailable errno was rejected")
		}
	}
	if isIPv6Unavailable(opaqueWrappedError{err: syscall.EADDRINUSE}) {
		t.Error("unexpected bind failure was treated as IPv6 absence")
	}
}
