//go:build windows

package secrets

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWindowsKeyringClientWaitsForSynchronousCallBeforeReturningCancellation(t *testing.T) {
	for _, operation := range []struct {
		name string
		call func(context.Context, keyringClient) error
	}{
		{
			name: "set",
			call: func(ctx context.Context, client keyringClient) error {
				return client.Set(ctx, "service", "account", "credential")
			},
		},
		{
			name: "get",
			call: func(ctx context.Context, client keyringClient) error {
				_, err := client.Get(ctx, "service", "account")
				return err
			},
		},
		{
			name: "delete",
			call: func(ctx context.Context, client keyringClient) error {
				return client.Delete(ctx, "service", "account")
			},
		},
	} {
		t.Run(operation.name, func(t *testing.T) {
			backend := newBlockingWindowsBackend()
			client := newWindowsKeyringClient(backend)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- operation.call(ctx, client) }()

			waitForKeyringSignal(t, backend.entered, "Windows backend entry")
			cancel()
			select {
			case <-result:
				t.Fatal("client returned while the synchronous native call was still running")
			default:
			}

			close(backend.release)
			var err error
			select {
			case err = <-result:
			case <-time.After(2 * time.Second):
				t.Fatal("client did not return after the synchronous native call completed")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", err)
			}
			select {
			case <-backend.completed:
			default:
				t.Fatal("client returned before native completion was recorded")
			}
		})
	}
}

type blockingWindowsBackend struct {
	entered   chan struct{}
	release   chan struct{}
	completed chan struct{}
}

func newBlockingWindowsBackend() *blockingWindowsBackend {
	return &blockingWindowsBackend{
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		completed: make(chan struct{}),
	}
}

func (b *blockingWindowsBackend) Set(string, string, string) error {
	return b.block()
}

func (b *blockingWindowsBackend) Get(string, string) (string, error) {
	return "", b.block()
}

func (b *blockingWindowsBackend) Delete(string, string) error {
	return b.block()
}

func (b *blockingWindowsBackend) block() error {
	close(b.entered)
	<-b.release
	close(b.completed)
	return nil
}
