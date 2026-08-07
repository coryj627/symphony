package workflow

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func intPointer(value int) *int          { return &value }
func stringPointer(value string) *string { return &value }

func TestStructuredSavePreservesCommentsUnknownKeysAndExactPromptBytes(t *testing.T) {
	source := "---\n# keep this comment\ntracker:\n  kind: github\n  provider:\n    owner: openai\n    repository: symphony\n    future_provider_key: keep\npolling:\n  interval_ms: 30000 # keep inline\nfuture_extension:\n  enabled: true\n---\n\nPrompt bytes stay exact.\n\n{{ issue.identifier }}\n"
	store, path := newWorkflowStore(t, source)
	before, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	promptBytes := []byte("\nPrompt bytes stay exact.\n\n{{ issue.identifier }}\n")

	got, err := store.Save(context.Background(), SaveCommand{
		BaseDigest: before.Digest,
		Patch:      &StructuredPatch{PollingIntervalMS: intPointer(45000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, retained := range []string{"# keep this comment", "# keep inline", "future_provider_key: keep", "future_extension:"} {
		if !strings.Contains(got.Source, retained) {
			t.Fatalf("structured save dropped %q", retained)
		}
	}
	if !strings.HasSuffix(got.Source, string(promptBytes)) {
		t.Fatalf("prompt bytes changed after structured save")
	}
	if got.Definition.Prompt != before.Definition.Prompt || got.Config.Polling.Interval.Milliseconds() != 45000 {
		t.Fatalf("unexpected structured result: prompt-preserved=%v interval=%d", got.Definition.Prompt == before.Definition.Prompt, got.Config.Polling.Interval.Milliseconds())
	}
	if disk, err := os.ReadFile(path); err != nil || string(disk) != got.Source {
		t.Fatalf("disk mismatch after save: %v", err)
	}
}

func TestStructuredSaveInsertsAbsentSupportedKeysWithoutRebuildingMappings(t *testing.T) {
	source := "---\ntracker:\n  kind: linear\n  provider:\n    project_slug: symphony\n    unknown: retained\n---\nKeep prompt exactly.\n"
	store, _ := newWorkflowStore(t, source)
	before, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Save(context.Background(), SaveCommand{
		BaseDigest: before.Digest,
		Patch: &StructuredPatch{
			WorkspaceRoot:    stringPointer("./workspaces"),
			ServerPort:       intPointer(43127),
			ProviderEndpoint: stringPointer("https://api.linear.app/graphql"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, inserted := range []string{"workspace:", "root: ./workspaces", "server:", "port: 43127", "endpoint: https://api.linear.app/graphql"} {
		if !strings.Contains(got.Source, inserted) {
			t.Fatalf("missing inserted field %q in structured source", inserted)
		}
	}
	if !strings.Contains(got.Source, "unknown: retained") || !strings.HasSuffix(got.Source, "Keep prompt exactly.\n") {
		t.Fatal("insertion rebuilt unknown data or prompt")
	}
}

func TestSaveRejectsExternalChange(t *testing.T) {
	for _, test := range []struct {
		name    string
		command func(Snapshot) SaveCommand
	}{
		{"raw", func(snapshot Snapshot) SaveCommand {
			return SaveCommand{BaseDigest: snapshot.Digest, RawSource: []byte(snapshot.Source)}
		}},
		{"structured", func(snapshot Snapshot) SaveCommand {
			return SaveCommand{BaseDigest: snapshot.Digest, Patch: &StructuredPatch{ServerPort: intPointer(1234)}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path := newWorkflowStore(t, validWorkflowSource)
			first, err := store.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			external := []byte(first.Source + "\nexternal change\n")
			if err := os.WriteFile(path, external, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = store.Save(context.Background(), test.command(first))
			if !errors.Is(err, ErrSaveConflict) {
				t.Fatalf("expected save conflict, got %v", err)
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(external) {
				t.Fatalf("conflicting save modified file: %v", readErr)
			}
		})
	}
}

func TestSaveRejectsInvalidRawAndStructuredCandidatesBeforeReplacement(t *testing.T) {
	for _, test := range []struct {
		name    string
		command func(Snapshot) SaveCommand
	}{
		{"raw", func(snapshot Snapshot) SaveCommand {
			return SaveCommand{BaseDigest: snapshot.Digest, RawSource: []byte("---\npolling:\n  interval_ms: -1\n---\nPrompt\n")}
		}},
		{"raw template", func(snapshot Snapshot) SaveCommand {
			return SaveCommand{BaseDigest: snapshot.Digest, RawSource: []byte("---\n---\n{{ unknown_binding }}\n")}
		}},
		{"structured", func(snapshot Snapshot) SaveCommand {
			return SaveCommand{BaseDigest: snapshot.Digest, Patch: &StructuredPatch{PollingIntervalMS: intPointer(-1)}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path := newWorkflowStore(t, validWorkflowSource)
			first, err := store.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			called := false
			ops := defaultAtomicOperations()
			originalReplace := ops.replace
			ops.replace = func(temp, destination string) error {
				called = true
				return originalReplace(temp, destination)
			}
			store.atomic = ops
			_, err = store.Save(context.Background(), test.command(first))
			if !errors.Is(err, ErrInvalidWorkflow) {
				t.Fatalf("expected invalid workflow, got %v", err)
			}
			if called {
				t.Fatal("invalid candidate reached atomic replacement")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("invalid candidate changed workflow")
			}
		})
	}
}

func TestRawSaveUsesExactSubmittedBytes(t *testing.T) {
	store, _ := newWorkflowStore(t, validWorkflowSource)
	first, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	candidate := []byte("---\r\ntracker:\r\n  kind: github\r\n  provider:\r\n    owner: openai\r\n    repository: symphony\r\n---\r\nExact raw bytes.\r\n")
	got, err := store.Save(context.Background(), SaveCommand{BaseDigest: first.Digest, RawSource: candidate})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != string(candidate) {
		t.Fatal("raw save normalized submitted bytes")
	}
}
