package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestKeyringStoreRoundTripAndCopiesReturnedValues(t *testing.T) {
	client := newFakeKeyringClient("symphony/workflow/workflow-id", "tracker/github")
	store := newKeyringForPlatform("symphony", "darwin", client)
	ref := Ref{WorkflowID: "workflow-id", TrackerKind: "github"}
	input := []byte("credential")

	if err := store.Put(context.Background(), ref, input); err != nil {
		t.Fatal(err)
	}
	clear(input)

	first, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first)
	if string(first) != "credential" {
		t.Fatal("first Get() returned an unexpected credential")
	}
	first[0] = 'X'

	second, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(second)
	if string(second) != "credential" {
		t.Fatal("second Get() returned an unexpected credential")
	}
}

func TestKeyringStorePutAvoidsRedundantCredentialCopy(t *testing.T) {
	store := &keyringStore{
		servicePrefix: "symphony",
		client:        discardKeyringClient{},
		supported:     true,
	}
	ctx := context.Background()
	ref := Ref{WorkflowID: "workflow-id", TrackerKind: "github"}
	credential := []byte("credential")

	allocations := testing.AllocsPerRun(1000, func() {
		if err := store.Put(ctx, ref, credential); err != nil {
			panic(err)
		}
	})
	if allocations > 3 {
		t.Fatalf("Put() allocations = %.0f, want at most 3", allocations)
	}
}

func TestKeyringStoreUsesIsolatedServicePrefix(t *testing.T) {
	client := newFakeKeyringClient("symphony-test/0123/workflow/w", "tracker/linear")
	store := newKeyringForPlatform("symphony-test/0123/", "darwin", client)
	ref := Ref{WorkflowID: "w", TrackerKind: "linear"}

	if err := store.Put(context.Background(), ref, []byte("credential")); err != nil {
		t.Fatal(err)
	}
}

func TestKeyringStoreMapsNotFoundSeparately(t *testing.T) {
	client := newFakeKeyringClient("symphony/workflow/w", "tracker/linear")
	store := newKeyringForPlatform("symphony", "darwin", client)
	ref := Ref{WorkflowID: "w", TrackerKind: "linear"}

	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(context.Background(), ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
	if got := store.Status(context.Background(), ref); got != (Status{Backend: "native-keyring", ErrorCode: "not_found"}) {
		t.Fatalf("Status() = %#v", got)
	}
}

func TestKeyringStoreDeleteRemovesCredential(t *testing.T) {
	client := newFakeKeyringClient("symphony/workflow/w", "tracker/github")
	store := newKeyringForPlatform("symphony", "darwin", client)
	ref := Ref{WorkflowID: "w", TrackerKind: "github"}

	if err := store.Put(context.Background(), ref, []byte("credential")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() after Delete() error = %v, want ErrNotFound", err)
	}
}

func TestKeyringStoreErrorsAndStatusNeverDiscloseCredential(t *testing.T) {
	const canary = "secret-canary"
	client := newFakeKeyringClient("symphony/workflow/w", "tracker/github")
	store := newKeyringForPlatform("symphony", "darwin", client)
	ref := Ref{WorkflowID: "w", TrackerKind: "github"}

	for _, operation := range []struct {
		name string
		run  func() string
	}{
		{
			name: "put error",
			run: func() string {
				client.setErr = errors.New("backend exposed " + canary)
				defer func() { client.setErr = nil }()
				return fmt.Sprint(store.Put(context.Background(), ref, []byte(canary)))
			},
		},
		{
			name: "get error",
			run: func() string {
				client.getErr = errors.New("backend exposed " + canary)
				defer func() { client.getErr = nil }()
				_, err := store.Get(context.Background(), ref)
				return fmt.Sprint(err)
			},
		},
		{
			name: "delete error",
			run: func() string {
				client.deleteErr = errors.New("backend exposed " + canary)
				defer func() { client.deleteErr = nil }()
				return fmt.Sprint(store.Delete(context.Background(), ref))
			},
		},
		{
			name: "status error",
			run: func() string {
				client.getErr = errors.New("backend exposed " + canary)
				defer func() { client.getErr = nil }()
				return fmt.Sprintf("%+v", store.Status(context.Background(), ref))
			},
		},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if got := operation.run(); strings.Contains(got, canary) {
				t.Fatal("credential disclosed by store boundary")
			}
		})
	}

	client.getErr = errors.New("backend unavailable")
	if got := store.Status(context.Background(), ref); got != (Status{Backend: "native-keyring", ErrorCode: "backend_error"}) {
		t.Fatalf("Status() = %#v", got)
	}
}

func TestKeyringStoreHonorsCanceledContextBeforeVaultAccess(t *testing.T) {
	client := newFakeKeyringClient("symphony/workflow/w", "tracker/github")
	store := newKeyringForPlatform("symphony", "darwin", client)
	ref := Ref{WorkflowID: "w", TrackerKind: "github"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Put(ctx, ref, []byte("credential")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put() error = %v, want context.Canceled", err)
	}
	if _, err := store.Get(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get() error = %v, want context.Canceled", err)
	}
	if err := store.Delete(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete() error = %v, want context.Canceled", err)
	}
	if got := store.Status(ctx, ref); got != (Status{Backend: "native-keyring", ErrorCode: "canceled"}) {
		t.Fatalf("Status() = %#v", got)
	}
	if client.accesses != 0 {
		t.Fatalf("vault access count = %d, want 0", client.accesses)
	}
}

func TestKeyringStoreRejectsUnsupportedPlatformWithoutBackendAccess(t *testing.T) {
	client := newFakeKeyringClient("symphony/workflow/w", "tracker/github")
	store := newKeyringForPlatform("symphony", "linux", client)
	ref := Ref{WorkflowID: "w", TrackerKind: "github"}
	credential := []byte("credential")
	defer clear(credential)

	if err := store.Put(context.Background(), ref, credential); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Put() error = %v, want ErrUnsupportedPlatform", err)
	}
	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Get() error = %v, want ErrUnsupportedPlatform", err)
	}
	if err := store.Delete(context.Background(), ref); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Delete() error = %v, want ErrUnsupportedPlatform", err)
	}
	if got := store.Status(context.Background(), ref); got != (Status{Backend: "native-keyring", ErrorCode: "unsupported_platform"}) {
		t.Fatalf("Status() = %#v", got)
	}
	if client.accesses != 0 {
		t.Fatalf("native backend access count = %d, want 0", client.accesses)
	}
}

func TestKeyringStorePropagatesCancellationAfterBackendEntryAndWaitsForCompletion(t *testing.T) {
	ref := Ref{WorkflowID: "w", TrackerKind: "github"}

	for _, operation := range []struct {
		name       string
		call       func(context.Context, Store) keyringCallResult
		wantStatus Status
	}{
		{
			name: "put",
			call: func(ctx context.Context, store Store) keyringCallResult {
				credential := []byte("credential")
				defer clear(credential)
				return keyringCallResult{err: store.Put(ctx, ref, credential)}
			},
		},
		{
			name: "get",
			call: func(ctx context.Context, store Store) keyringCallResult {
				value, err := store.Get(ctx, ref)
				clear(value)
				return keyringCallResult{err: err}
			},
		},
		{
			name: "delete",
			call: func(ctx context.Context, store Store) keyringCallResult {
				return keyringCallResult{err: store.Delete(ctx, ref)}
			},
		},
		{
			name: "status",
			call: func(ctx context.Context, store Store) keyringCallResult {
				return keyringCallResult{status: store.Status(ctx, ref)}
			},
			wantStatus: Status{Backend: "native-keyring", ErrorCode: "canceled"},
		},
	} {
		t.Run(operation.name, func(t *testing.T) {
			client := newBlockingKeyringClient()
			store := newKeyringForPlatform("symphony", "darwin", client)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan keyringCallResult, 1)
			go func() { result <- operation.call(ctx, store) }()

			waitForKeyringSignal(t, client.entered, "native backend entry")
			cancel()
			waitForKeyringSignal(t, client.cancellationObserved, "backend cancellation observation")
			select {
			case <-result:
				t.Fatal("store returned before the native call completed")
			default:
			}

			close(client.release)
			var got keyringCallResult
			select {
			case got = <-result:
			case <-time.After(2 * time.Second):
				t.Fatal("store did not return after native completion")
			}
			select {
			case <-client.completed:
			default:
				t.Fatal("store returned before native completion was recorded")
			}
			if operation.wantStatus != (Status{}) {
				if got.status != operation.wantStatus {
					t.Fatalf("Status() = %#v", got.status)
				}
			} else if !errors.Is(got.err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", got.err)
			}
		})
	}
}

func waitForKeyringSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

type keyringCallResult struct {
	err    error
	status Status
}

type blockingKeyringClient struct {
	entered              chan struct{}
	cancellationObserved chan struct{}
	release              chan struct{}
	completed            chan struct{}
}

func newBlockingKeyringClient() *blockingKeyringClient {
	return &blockingKeyringClient{
		entered:              make(chan struct{}),
		cancellationObserved: make(chan struct{}),
		release:              make(chan struct{}),
		completed:            make(chan struct{}),
	}
}

func (c *blockingKeyringClient) Set(ctx context.Context, _, _, _ string) error {
	return c.block(ctx)
}

func (c *blockingKeyringClient) Get(ctx context.Context, _, _ string) (string, error) {
	return "", c.block(ctx)
}

func (c *blockingKeyringClient) Delete(ctx context.Context, _, _ string) error {
	return c.block(ctx)
}

func (c *blockingKeyringClient) block(ctx context.Context) error {
	close(c.entered)
	<-ctx.Done()
	close(c.cancellationObserved)
	<-c.release
	close(c.completed)
	return ctx.Err()
}

type fakeKeyringClient struct {
	wantService string
	wantAccount string
	password    string
	present     bool
	setErr      error
	getErr      error
	deleteErr   error
	accesses    int
}

type discardKeyringClient struct{}

func (discardKeyringClient) Set(context.Context, string, string, string) error { return nil }
func (discardKeyringClient) Get(context.Context, string, string) (string, error) {
	return "", ErrNotFound
}
func (discardKeyringClient) Delete(context.Context, string, string) error { return ErrNotFound }

func newFakeKeyringClient(service, account string) *fakeKeyringClient {
	return &fakeKeyringClient{wantService: service, wantAccount: account}
}

func (c *fakeKeyringClient) Set(_ context.Context, service, account, password string) error {
	c.accesses++
	if err := c.checkNames(service, account); err != nil {
		return err
	}
	if c.setErr != nil {
		return c.setErr
	}
	c.password = password
	c.present = true
	return nil
}

func (c *fakeKeyringClient) Get(_ context.Context, service, account string) (string, error) {
	c.accesses++
	if err := c.checkNames(service, account); err != nil {
		return "", err
	}
	if c.getErr != nil {
		return "", c.getErr
	}
	if !c.present {
		return "", ErrNotFound
	}
	return c.password, nil
}

func (c *fakeKeyringClient) Delete(_ context.Context, service, account string) error {
	c.accesses++
	if err := c.checkNames(service, account); err != nil {
		return err
	}
	if c.deleteErr != nil {
		return c.deleteErr
	}
	if !c.present {
		return ErrNotFound
	}
	c.password = ""
	c.present = false
	return nil
}

func (c *fakeKeyringClient) checkNames(service, account string) error {
	if service != c.wantService || account != c.wantAccount {
		return fmt.Errorf("unexpected keyring names: %q %q", service, account)
	}
	return nil
}
