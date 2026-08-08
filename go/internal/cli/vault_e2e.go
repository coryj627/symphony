//go:build e2e

package cli

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/coryj627/symphony/go/internal/secrets"
)

type e2eVault struct {
	mu     sync.Mutex
	values map[secrets.Ref][]byte
}

var browserTestVault = &e2eVault{values: make(map[secrets.Ref][]byte)}

func newVaultStore() secrets.Store { return browserTestVault }

func (vault *e2eVault) Put(ctx context.Context, ref secrets.Ref, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	vault.mu.Lock()
	defer vault.mu.Unlock()
	clear(vault.values[ref])
	vault.values[ref] = append([]byte(nil), value...)
	return nil
}

func (vault *e2eVault) Get(ctx context.Context, ref secrets.Ref) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vault.mu.Lock()
	defer vault.mu.Unlock()
	value, ok := vault.values[ref]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func (vault *e2eVault) Delete(ctx context.Context, ref secrets.Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ref.TrackerKind == os.Getenv("SYMPHONY_E2E_FAIL_DELETE_TRACKER") {
		return errors.New("e2e_delete_failed")
	}
	vault.mu.Lock()
	defer vault.mu.Unlock()
	value, ok := vault.values[ref]
	if !ok {
		return secrets.ErrNotFound
	}
	clear(value)
	delete(vault.values, ref)
	return nil
}

func (vault *e2eVault) Status(ctx context.Context, ref secrets.Ref) secrets.Status {
	if err := ctx.Err(); err != nil {
		return secrets.Status{Backend: "native-keyring", ErrorCode: "canceled"}
	}
	vault.mu.Lock()
	defer vault.mu.Unlock()
	_, ok := vault.values[ref]
	if !ok {
		return secrets.Status{Backend: "native-keyring", ErrorCode: "not_found"}
	}
	return secrets.Status{Present: true, Backend: "native-keyring"}
}
