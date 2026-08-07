package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	nativekeyring "github.com/zalando/go-keyring"
)

func TestKeyringStoreRoundTripAndCopiesReturnedValues(t *testing.T) {
	client := newFakeKeyringClient("symphony/workflow/workflow-id", "tracker/github")
	store := newKeyring("symphony", client)
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

func TestKeyringStoreUsesIsolatedServicePrefix(t *testing.T) {
	client := newFakeKeyringClient("symphony-test/0123/workflow/w", "tracker/linear")
	store := newKeyring("symphony-test/0123/", client)
	ref := Ref{WorkflowID: "w", TrackerKind: "linear"}

	if err := store.Put(context.Background(), ref, []byte("credential")); err != nil {
		t.Fatal(err)
	}
}

func TestKeyringStoreMapsNotFoundSeparately(t *testing.T) {
	client := newFakeKeyringClient("symphony/workflow/w", "tracker/linear")
	store := newKeyring("symphony", client)
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
	store := newKeyring("symphony", client)
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
	store := newKeyring("symphony", client)
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
	store := newKeyring("symphony", client)
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

func newFakeKeyringClient(service, account string) *fakeKeyringClient {
	return &fakeKeyringClient{wantService: service, wantAccount: account}
}

func (c *fakeKeyringClient) Set(service, account, password string) error {
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

func (c *fakeKeyringClient) Get(service, account string) (string, error) {
	c.accesses++
	if err := c.checkNames(service, account); err != nil {
		return "", err
	}
	if c.getErr != nil {
		return "", c.getErr
	}
	if !c.present {
		return "", nativekeyring.ErrNotFound
	}
	return c.password, nil
}

func (c *fakeKeyringClient) Delete(service, account string) error {
	c.accesses++
	if err := c.checkNames(service, account); err != nil {
		return err
	}
	if c.deleteErr != nil {
		return c.deleteErr
	}
	if !c.present {
		return nativekeyring.ErrNotFound
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
