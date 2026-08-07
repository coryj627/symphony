//go:build darwin

package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMacOSKeyringSetUsesCompatibleEncodingWithoutSecretArguments(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "marker")
	const canary = "secret-canary"
	runner := &recordingSecurityRunner{
		run: func(gotCtx context.Context, stdin []byte, path string, args ...string) ([]byte, error) {
			if gotCtx.Value(contextKey{}) != "marker" {
				t.Fatal("Set did not propagate its context to the security runner")
			}
			if path != "/usr/bin/security" || !reflect.DeepEqual(args, []string{"-i"}) {
				t.Fatalf("security invocation = %q %#v", path, args)
			}
			if strings.Contains(strings.Join(args, " "), canary) {
				t.Fatal("credential appeared in security process arguments")
			}
			want := "add-generic-password -U -s service -a account -w go-keyring-base64:c2VjcmV0LWNhbmFyeQ==\n"
			if string(stdin) != want {
				t.Fatal("security input did not use the go-keyring-compatible encoding")
			}
			return nil, nil
		},
	}
	client := newMacOSKeyringClient(runner)

	if err := client.Set(ctx, "service", "account", canary); err != nil {
		t.Fatal(err)
	}
}

func TestMacOSKeyringGetDecodesCompatibleValueAndMapsNotFound(t *testing.T) {
	t.Run("compatible value", func(t *testing.T) {
		runner := &recordingSecurityRunner{
			run: func(_ context.Context, stdin []byte, path string, args ...string) ([]byte, error) {
				if len(stdin) != 0 {
					t.Fatal("Get supplied unexpected standard input")
				}
				wantArgs := []string{"find-generic-password", "-s", "service", "-wa", "account"}
				if path != "/usr/bin/security" || !reflect.DeepEqual(args, wantArgs) {
					t.Fatalf("security invocation = %q %#v", path, args)
				}
				return []byte("go-keyring-base64:c2VjcmV0LWNhbmFyeQ==\n"), nil
			},
		}
		client := newMacOSKeyringClient(runner)

		got, err := client.Get(context.Background(), "service", "account")
		if err != nil {
			t.Fatal(err)
		}
		if got != "secret-canary" {
			t.Fatal("Get returned an unexpected credential")
		}
	})

	t.Run("not found", func(t *testing.T) {
		runner := &recordingSecurityRunner{
			run: func(context.Context, []byte, string, ...string) ([]byte, error) {
				return []byte("The specified item could not be found in the keychain."), errors.New("security failed")
			},
		}
		client := newMacOSKeyringClient(runner)

		if _, err := client.Get(context.Background(), "service", "account"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get() error = %v, want ErrNotFound", err)
		}
	})
}

func TestExecSecurityRunnerCancelsStartedProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := (execSecurityRunner{}).Run(ctx, nil, "/bin/sh", "-c", "touch \"$1\"; exec sleep 30", "security-test", marker)
		result <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper process did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runner error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("security runner remained blocked after cancellation")
	}
}

type recordingSecurityRunner struct {
	run func(context.Context, []byte, string, ...string) ([]byte, error)
}

func (r *recordingSecurityRunner) Run(ctx context.Context, stdin []byte, path string, args ...string) ([]byte, error) {
	return r.run(ctx, stdin, path, args...)
}
