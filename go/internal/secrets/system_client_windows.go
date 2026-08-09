//go:build windows

package secrets

import (
	"context"
	"errors"

	nativekeyring "github.com/zalando/go-keyring"
)

// windowsKeyringClient uses synchronous Credential Manager APIs. Windows
// exposes no cancellation primitive for these calls, so each method checks the
// context immediately before and after the native call and never returns while
// a mutation is still running.
type windowsCredentialBackend interface {
	Set(string, string, string) error
	Get(string, string) (string, error)
	Delete(string, string) error
}

type goKeyringWindowsBackend struct{}

func (goKeyringWindowsBackend) Set(service, account, password string) error {
	return nativekeyring.Set(service, account, password)
}

func (goKeyringWindowsBackend) Get(service, account string) (string, error) {
	return nativekeyring.Get(service, account)
}

func (goKeyringWindowsBackend) Delete(service, account string) error {
	return nativekeyring.Delete(service, account)
}

type windowsKeyringClient struct {
	backend windowsCredentialBackend
}

func newSystemKeyringClient() keyringClient {
	return newWindowsKeyringClient(goKeyringWindowsBackend{})
}

func newWindowsKeyringClient(backend windowsCredentialBackend) keyringClient {
	return &windowsKeyringClient{backend: backend}
}

func (c *windowsKeyringClient) Set(ctx context.Context, service, account, password string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := c.backend.Set(service, account, password)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func (c *windowsKeyringClient) Get(ctx context.Context, service, account string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, err := c.backend.Get(service, account)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if errors.Is(err, nativekeyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return value, err
}

func (c *windowsKeyringClient) Delete(ctx context.Context, service, account string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := c.backend.Delete(service, account)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, nativekeyring.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
