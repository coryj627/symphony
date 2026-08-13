package codex

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/observability"
)

func TestStderrCaptureRedactsAndKeepsOnlyBoundedTail(t *testing.T) {
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(secret))
	capture := NewStderrCapture(redactor, nil)
	input := strings.Repeat("x", maxStderrBytes+1024) + "\nsecret=" + secret + "\n"
	if written, err := capture.Write([]byte(input)); err != nil || written != len(input) {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	diagnostic := capture.Diagnostic()
	if len(diagnostic) > maxStderrBytes {
		t.Fatalf("diagnostic retained %d bytes", len(diagnostic))
	}
	if strings.Contains(diagnostic, secret) || !strings.Contains(diagnostic, "[REDACTED]") {
		t.Fatalf("diagnostic was not redacted: %q", diagnostic)
	}
}

func TestStderrCaptureLogsOnlySanitizedSummaries(t *testing.T) {
	const secret = "lin_api_abcdefghijklmnopqrstuvwxyz"
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(secret))
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	capture := NewStderrCapture(redactor, logger)
	_, _ = capture.Write([]byte("first " + secret + "\nsecond\n"))
	capture.LogSummary()
	logged := output.String()
	if strings.Contains(logged, secret) || !strings.Contains(logged, "codex_stderr") || !strings.Contains(logged, "[REDACTED]") {
		t.Fatalf("unsafe stderr log: %q", logged)
	}
}

func TestStderrCaptureHandlesSecretSplitAcrossWrites(t *testing.T) {
	const secret = "sk-abcdefghijklmnopqrstuvwxyz012345"
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(secret))
	capture := NewStderrCapture(redactor, nil)
	_, _ = capture.Write([]byte(secret[:12]))
	_, _ = capture.Write([]byte(secret[12:] + "\n"))
	if diagnostic := capture.Diagnostic(); strings.Contains(diagnostic, secret) || !strings.Contains(diagnostic, "[REDACTED]") {
		t.Fatalf("split secret was not redacted: %q", diagnostic)
	}
}
