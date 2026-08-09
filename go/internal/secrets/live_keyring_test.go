//go:build keyring_live

package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"testing"
)

func TestLiveKeyringRoundTrip(t *testing.T) {
	if os.Getenv("SYMPHONY_RUN_KEYRING_LIVE") != "1" {
		t.Skip("set SYMPHONY_RUN_KEYRING_LIVE=1 to access the native credential vault")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("native credential-vault smoke test requires macOS or Windows")
	}

	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("create isolated keyring service: %v", err)
	}
	servicePrefix := "symphony-test/" + hex.EncodeToString(suffix[:])
	store := NewKeyring(servicePrefix)
	ref := Ref{WorkflowID: "live", TrackerKind: "test"}
	ctx := context.Background()
	t.Cleanup(func() {
		if err := store.Delete(ctx, ref); err != nil && !errors.Is(err, ErrNotFound) {
			t.Errorf("clean up isolated keyring credential: %v", err)
		}
	})

	canary := []byte("symphony-keyring-live-canary")
	defer clear(canary)
	if err := store.Put(ctx, ref, canary); err != nil {
		t.Fatalf("write isolated keyring credential: %v", err)
	}

	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("read isolated keyring credential: %v", err)
	}
	defer clear(got)
	if !bytes.Equal(got, canary) {
		t.Fatal("native keyring returned a different credential")
	}

	if err := store.Delete(ctx, ref); err != nil {
		t.Fatalf("delete isolated keyring credential: %v", err)
	}
	afterDelete, err := store.Get(ctx, ref)
	clear(afterDelete)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after delete: got %v, want ErrNotFound", err)
	}
}
