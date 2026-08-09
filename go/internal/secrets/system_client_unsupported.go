//go:build !darwin && !windows

package secrets

import "context"

type unsupportedSystemKeyringClient struct{}

func newSystemKeyringClient() keyringClient {
	return unsupportedSystemKeyringClient{}
}

func (unsupportedSystemKeyringClient) Set(context.Context, string, string, string) error {
	return ErrUnsupportedPlatform
}

func (unsupportedSystemKeyringClient) Get(context.Context, string, string) (string, error) {
	return "", ErrUnsupportedPlatform
}

func (unsupportedSystemKeyringClient) Delete(context.Context, string, string) error {
	return ErrUnsupportedPlatform
}
