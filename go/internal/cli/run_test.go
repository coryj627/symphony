package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestRunReturnsArgumentFailureForInvalidFlags(t *testing.T) {
	var stderr bytes.Buffer

	if got := Run(context.Background(), []string{"--port", "70000"}, io.Discard, &stderr); got != 2 {
		t.Fatalf("Run() = %d, want 2", got)
	}
	if stderr.Len() == 0 {
		t.Fatal("Run() did not report the argument error")
	}
}

func TestRunReturnsStartupFailure(t *testing.T) {
	restoreStart(t, func(context.Context, Options, io.Writer, io.Writer) error {
		return errors.New("startup failed")
	})
	var stderr bytes.Buffer

	if got := Run(context.Background(), nil, io.Discard, &stderr); got != 1 {
		t.Fatalf("Run() = %d, want 1", got)
	}
	if got := stderr.String(); got != "startup failed\n" {
		t.Fatalf("stderr = %q, want startup error", got)
	}
}

func TestRunReturnsRuntimeFailureWhenStartupEndsBeforeShutdown(t *testing.T) {
	restoreStart(t, func(context.Context, Options, io.Writer, io.Writer) error {
		return nil
	})
	var stderr bytes.Buffer

	if got := Run(context.Background(), nil, io.Discard, &stderr); got != 1 {
		t.Fatalf("Run() = %d, want 1", got)
	}
	if stderr.Len() == 0 {
		t.Fatal("Run() did not report the unexpected startup completion")
	}
}

func TestRunReturnsSuccessAfterContextDrivenShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restoreStart(t, func(gotCtx context.Context, opts Options, stdout, _ io.Writer) error {
		if gotCtx != ctx {
			return errors.New("unexpected context")
		}
		if opts.Mode != ModeConfigure || opts.WorkflowPath != "C:/work/WORKFLOW.md" {
			return fmt.Errorf("unexpected options: %#v", opts)
		}
		if _, err := fmt.Fprintln(stdout, "stopped cleanly"); err != nil {
			return err
		}
		return nil
	})
	var stdout, stderr bytes.Buffer

	if got := Run(ctx, []string{"configure", "C:/work/WORKFLOW.md"}, &stdout, &stderr); got != 0 {
		t.Fatalf("Run() = %d, want 0; stderr = %q", got, stderr.String())
	}
	if got := stdout.String(); got != "stopped cleanly\n" {
		t.Fatalf("stdout = %q, want clean shutdown output", got)
	}
}

func restoreStart(t *testing.T, replacement func(context.Context, Options, io.Writer, io.Writer) error) {
	t.Helper()
	previous := start
	start = replacement
	t.Cleanup(func() { start = previous })
}
