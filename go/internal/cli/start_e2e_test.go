//go:build e2e

package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestE2EStartComposesProtectedPageServerWithoutScheduler(t *testing.T) {
	const bootstrapToken = "test-only-capability-value-0123456789abcdef0123456789abcdef"
	t.Setenv("SYMPHONY_E2E_BOOTSTRAP_TOKEN", bootstrapToken)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- startE2E(ctx, Options{Mode: ModeConfigure, Port: port}, io.Discard, io.Discard)
	}()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: time.Second}
	var response *http.Response
	for attempt := 0; attempt < 100; attempt++ {
		response, err = client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/?access_token=" + bootstrapToken)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatal("e2e page server did not become available")
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		cancel()
		t.Fatal("could not read e2e page response")
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>Overview — Symphony</title>") {
		cancel()
		t.Fatalf("e2e page response was not the rendered overview")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("e2e startup did not shut down cleanly: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("e2e startup did not stop after cancellation")
	}
}

func TestE2EStartRejectsProductionRunMode(t *testing.T) {
	if err := startE2E(context.Background(), Options{Mode: ModeRun}, io.Discard, io.Discard); err == nil {
		t.Fatal("e2e startup accepted run mode")
	}
}
