package instance

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAcquireReplacesMetadataFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	info := testInfo(root, "data")
	if err := os.MkdirAll(info.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(info.DataDir, "instance.json")
	if err := unix.Mkfifo(metadataPath, 0o600); err != nil {
		t.Fatal(err)
	}

	result := make(chan struct {
		lock *Lock
		err  error
	}, 1)
	go func() {
		lock, err := Acquire(info)
		result <- struct {
			lock *Lock
			err  error
		}{lock: lock, err: err}
	}()

	select {
	case acquired := <-result:
		if acquired.err != nil {
			t.Fatal(acquired.err)
		}
		if err := acquired.lock.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Acquire blocked while opening existing metadata FIFO")
	}
}
