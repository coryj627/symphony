package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/security"
)

func TestCanaryNeverCrossesSecretBoundaryArtifacts(t *testing.T) {
	canary := security.NewCanary(t)
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(canary.Value))

	dataDirectory := privateTempDir(t)
	logger, logStore, err := observability.NewLogger(observability.Options{
		DataDir:  dataDirectory,
		Redactor: redactor,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("NewLogger() failed: %v", err)
	}

	const safeIssue = "SAFE-123"
	logger.Info("secret boundary "+canary.Value, "credential", canary.Value, "issue", safeIssue)
	stderr := codex.NewStderrCapture(redactor, logger)
	if _, err := stderr.Write([]byte("provider stderr=" + canary.Value + "\n")); err != nil {
		t.Fatalf("stderr Write() failed: %v", err)
	}
	stderr.LogSummary()

	snapshotValue := redactor.Value(map[string]any{
		"issue":      safeIssue,
		"credential": canary.Value,
	})
	snapshot, err := json.Marshal(snapshotValue)
	if err != nil {
		t.Fatalf("snapshot marshal failed: %v", err)
	}

	response := httptest.NewRecorder()
	response.Header().Set("X-Symphony-Diagnostic", redactor.Value("Bearer "+canary.Value).(string))
	response.Header().Set("Content-Type", "application/json")
	if _, err := response.Write(snapshot); err != nil {
		t.Fatalf("HTTP response write failed: %v", err)
	}
	httpResponse := response.Result()
	defer httpResponse.Body.Close()
	httpBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		t.Fatalf("HTTP response read failed: %v", err)
	}
	httpHeaders, err := json.Marshal(httpResponse.Header)
	if err != nil {
		t.Fatalf("HTTP header marshal failed: %v", err)
	}
	sse := append([]byte("event: snapshot\ndata: "), httpBody...)
	sse = append(sse, []byte("\n\n")...)

	journal := observability.NewJournal(observability.JournalOptions{})
	defer journal.Close()
	eventData, ok := snapshotValue.(map[string]any)
	if !ok {
		t.Fatal("redacted snapshot had an unexpected type")
	}
	if _, err := journal.Publish(domain.Event{Type: "snapshot", Data: eventData}); err != nil {
		t.Fatalf("journal Publish() failed: %v", err)
	}
	events, err := json.Marshal(journal.Recent(1))
	if err != nil {
		t.Fatalf("journal marshal failed: %v", err)
	}

	childEnvironment := codex.SanitizeEnvironment(
		[]string{"PATH=C:\\Windows", "SYMPHONY_TEST_CREDENTIAL=" + canary.Value, "SAFE=yes"},
		[]string{"SYMPHONY_TEST_CREDENTIAL"},
	)
	if !slices.Contains(childEnvironment, "SAFE=yes") {
		t.Fatal("child environment removed a safe control value")
	}

	logPage, err := logStore.Query(context.Background(), observability.LogQuery{})
	if err != nil {
		t.Fatalf("log Query() failed: %v", err)
	}
	logs, err := json.Marshal(logPage)
	if err != nil {
		t.Fatalf("log marshal failed: %v", err)
	}
	if err := logStore.Close(); err != nil {
		t.Fatalf("log Close() failed: %v", err)
	}
	if logStore.Degraded() {
		t.Fatal("structured log sink degraded during secret-boundary scenario")
	}
	if info, err := os.Stat(filepath.Join(dataDirectory, "logs", "symphony.jsonl")); err != nil || !info.Mode().IsRegular() {
		t.Fatal("secret-boundary scenario did not produce a regular structured log artifact")
	}

	artifactDirectory := filepath.Join(dataDirectory, "captured-test-artifacts")
	if err := os.Mkdir(artifactDirectory, 0o700); err != nil {
		t.Fatalf("artifact directory creation failed: %v", err)
	}
	for name, content := range map[string][]byte{
		"http-body.json":    httpBody,
		"http-headers.json": httpHeaders,
		"events.json":       events,
		"snapshot.json":     snapshot,
		"stream.txt":        sse,
		"logs.json":         logs,
	} {
		if err := os.WriteFile(filepath.Join(artifactDirectory, name), content, 0o600); err != nil {
			t.Fatalf("captured artifact write failed: %v", err)
		}
	}

	combined := strings.Join([]string{
		string(httpBody), string(events), string(snapshot), string(logs),
	}, "\n")
	if !strings.Contains(combined, safeIssue) {
		t.Fatal("sanitization removed safe observable issue content")
	}

	canary.AssertAbsent(t,
		security.BytesArtifact("HTTP body", httpBody),
		security.BytesArtifact("HTTP headers", httpHeaders),
		security.BytesArtifact("SSE stream", sse),
		security.BytesArtifact("snapshot", snapshot),
		security.BytesArtifact("event journal", events),
		security.BytesArtifact("structured logs", logs),
		security.TextArtifact("stderr capture", stderr.Diagnostic()),
		security.StringsArtifact("child environment", childEnvironment),
		security.PathArtifact("data and captured artifact directory", dataDirectory),
	)
}
