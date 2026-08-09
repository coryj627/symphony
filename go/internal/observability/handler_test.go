package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestLoggerRedactsMessagesAttributesErrorsAndURLs(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	redactor := NewRedactor(nil, nil)
	logger, store, err := NewLogger(Options{DataDir: dataDir, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	redactor.RegisterSecret([]byte(testCanary))
	logger.Error(
		"failed "+testCanary,
		"authorization", "Bearer "+testCanary,
		"url", "http://127.0.0.1/?access_token="+testCanary+"&page=2",
		"error", errors.New("wrapped "+testCanary),
	)

	page, err := store.Query(context.Background(), LogQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("record count = %d, want 1", len(page.Records))
	}
	record := page.Records[0]
	assertNoCanary(t, record)
	if !strings.Contains(record.Message, "failed") || !strings.Contains(toJSON(record.Fields), "[REDACTED]") {
		t.Fatalf("safe diagnostic context missing from record")
	}
}

func TestLoggerResanitizesReconstructedURLAcrossMessageKeyAndValue(t *testing.T) {
	t.Parallel()

	reconstructed := "https:\x1b[31m//alice:ordinary-password@example.test/path?access_token=short-secret#capability-fragment"
	logger, store, err := NewLogger(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger.Info(reconstructed, reconstructed, reconstructed)
	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("record count = %d, want 1", len(page.Records))
	}
	assertURLCredentialsAbsent(t, safeSprint(page.Records[0]))
}

func TestNewLoggerRejectsOversizedJSONLineButRetainsRing(t *testing.T) {
	dataDir := t.TempDir()
	var warnings strings.Builder
	logger, store, err := NewLogger(Options{DataDir: dataDir, Stderr: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	attrs := make([]any, 0, 400)
	for index := 0; index < 200; index++ {
		attrs = append(attrs, fmt.Sprintf("field-%03d", index), strings.Repeat("x", maxSanitizedBytes))
	}
	logger.Info("oversized", attrs...)
	if !store.Degraded() {
		t.Fatal("oversized public log record did not degrade the file sink")
	}
	if warnings.String() != degradationWarning+"\n" {
		t.Fatal("oversized public log record did not emit exactly one static warning")
	}
	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].Message != "oversized" {
		t.Fatal("oversized public log record was not retained in the ring")
	}
	active := filepath.Join(dataDir, "logs", "symphony.jsonl")
	info, err := os.Stat(active)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatal("oversized public log record partially wrote the active file")
	}
	if _, err := os.Stat(active + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversized public log record created an archive")
	}

	logger.Info("small-after-failure")
	if warnings.String() != degradationWarning+"\n" {
		t.Fatal("sticky degradation emitted more than one warning")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	logger.Info("after-close")
	page, err = store.Query(context.Background(), LogQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 3 || !page.Degraded {
		t.Fatal("ring did not survive oversized failure and close")
	}
}

func TestLoggerProtectsAlreadyDerivedLoggersAfterLateRegistration(t *testing.T) {
	t.Parallel()

	values := map[string]string{"RELOADED_TOKEN": testCanary}
	redactor := NewRedactor(nil, nil)
	logger, store, err := NewLogger(Options{DataDir: t.TempDir(), Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	derived := logger.With("captured", testCanary).WithGroup("details").With("nested", testCanary)
	redactor.RegisterEnvironmentNames([]string{"RELOADED_TOKEN"}, func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	})
	derived.Info("late "+testCanary, "RELOADED_TOKEN", testCanary)

	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("record count = %d, want 1", len(page.Records))
	}
	assertNoCanary(t, page.Records[0])
}

func TestHandlerOwnsDerivedAttributeSlices(t *testing.T) {
	t.Parallel()

	logger, store, err := NewLogger(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	children := []slog.Attr{slog.String("value", "original")}
	attrs := []slog.Attr{{Key: "group", Value: slog.GroupValue(children...)}}
	derived := slog.New(logger.Handler().WithAttrs(attrs))
	children[0] = slog.String("value", testCanary)
	attrs[0] = slog.String("mutated", testCanary)
	derived.Info("owned")

	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoCanary(t, page.Records)
	group, ok := page.Records[0].Fields["group"].(map[string]any)
	if !ok || group["value"] != "original" {
		t.Fatal("derived attribute slices were not owned by the handler")
	}
}

func TestLoggerPreservesGroupsAndOmitsEmptyGroups(t *testing.T) {
	t.Parallel()

	logger, store, err := NewLogger(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger.With("root", 1).
		WithGroup("").
		WithGroup("outer").
		With("inside", 2).
		Info("grouped",
			slog.Group("inline", slog.String("", "ignored-key-value"), slog.String("named", "yes")),
			slog.Group("empty"),
			slog.Group("Cookie", slog.String("token", testCanary)),
		)

	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	fields := page.Records[0].Fields
	assertNoCanary(t, fields)
	if fields["root"] != float64(1) && fields["root"] != int64(1) && fields["root"] != 1 {
		t.Fatalf("root field = %#v", fields["root"])
	}
	outer, ok := fields["outer"].(map[string]any)
	if !ok || outer["inside"] == nil || outer["empty"] != nil {
		t.Fatal("outer group structure was not preserved")
	}
	if outer["Cookie"] != redactedMarker {
		t.Fatal("sensitive group was not replaced with a redacted marker")
	}
}

func TestLoggerRecognizesSensitiveKeysAfterControlSanitization(t *testing.T) {
	t.Parallel()

	logger, store, err := NewLogger(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger.Info("safe", "to\rken", "credential-value-that-is-not-a-known-shape")
	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Records[0].Fields["token"] != redactedMarker {
		t.Fatal("sanitized sensitive key value was not redacted")
	}
}

func TestLoggerBoundsCompositeGroupFields(t *testing.T) {
	t.Parallel()

	logger, store, err := NewLogger(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	attrs := make([]any, 0, 130)
	for index := 0; index < 65; index++ {
		attrs = append(attrs, "field", strings.Repeat("x", 2048))
	}
	logger.Info("bounded", slog.Group("oversized", attrs...))
	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Records[0].Fields["oversized"] != truncationMarker {
		t.Fatalf("oversized group was retained as %T", page.Records[0].Fields["oversized"])
	}
}

func TestLoggerWritesStandardJSONAndConcurrentLines(t *testing.T) {
	dataDir := t.TempDir()
	logger, store, err := NewLogger(Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}

	const count = 64
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			logger.Info("concurrent", "index", index)
		}(index)
	}
	wait.Wait()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dataDir, "logs", "symphony.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != count {
		t.Fatalf("line count = %d, want %d", len(lines), count)
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid JSON line: %v", err)
		}
		for _, key := range []string{"time", "level", "msg", "index"} {
			if _, ok := record[key]; !ok {
				t.Fatalf("JSON record lacks %q", key)
			}
		}
	}
}

func TestNewLoggerCreatesPrivateDirectoriesAndFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX permission bits")
	}
	dataDir := filepath.Join(t.TempDir(), "new-data")
	logger, store, err := NewLogger(Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("permissions")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(dataDir, "logs"):                   0o700,
		filepath.Join(dataDir, "logs", "symphony.jsonl"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s permissions = %04o, want %04o", filepath.Base(path), info.Mode().Perm(), want)
		}
	}
}

func TestNewLoggerValidatesDataDirectoryAndDegradesSafely(t *testing.T) {
	t.Parallel()

	if _, _, err := NewLogger(Options{DataDir: "relative"}); err == nil {
		t.Fatal("relative data directory accepted")
	}

	dataDir := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dataDir, "logs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var warnings strings.Builder
	logger, store, err := NewLogger(Options{DataDir: dataDir, Stderr: &warnings})
	if err != nil {
		t.Fatalf("filesystem failure returned constructor error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger.Info("first")
	logger.Info("second")
	if !store.Degraded() {
		t.Fatal("store did not enter degraded state")
	}
	if warnings.String() != degradationWarning+"\n" {
		t.Fatalf("warning count = %d, want exactly one static warning", strings.Count(warnings.String(), degradationWarning))
	}
	page, queryErr := store.Query(context.Background(), LogQuery{})
	if queryErr != nil || len(page.Records) != 2 {
		t.Fatalf("ring query after degradation = %d records, %v", len(page.Records), queryErr)
	}
}

func TestLoggerAndSecretRegistrationAreRaceSafe(t *testing.T) {
	redactor := NewRedactor(nil, nil)
	logger, store, err := NewLogger(Options{DataDir: t.TempDir(), Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 100; index++ {
				redactor.RegisterSecret([]byte(testCanary))
				logger.Info("race "+testCanary, "worker", worker, "index", index)
			}
		}(worker)
	}
	wait.Wait()
	page, err := store.Query(context.Background(), LogQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range page.Records {
		assertNoCanary(t, record)
	}
}
