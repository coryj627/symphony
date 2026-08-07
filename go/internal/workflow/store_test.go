package workflow

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validWorkflowSource = "---\ntracker:\n  kind: github\n  provider:\n    owner: openai\n    repository: symphony\n---\nWork on {{ issue.identifier }}.\n"

func newWorkflowStore(t *testing.T, source string) (*FileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if source != "" {
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(context.Background(), path, os.LookupEnv, func(EffectiveConfig) []FieldError { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func TestStoreLoadComputesDigestFromExactSourceBytes(t *testing.T) {
	store, _ := newWorkflowStore(t, validWorkflowSource)
	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(validWorkflowSource)))
	if snapshot.Digest != want || snapshot.Source != validWorkflowSource {
		t.Fatalf("snapshot did not retain exact source or digest: digest=%q", snapshot.Digest)
	}
	current, ok := store.Current()
	if !ok || current.Digest != want {
		t.Fatalf("current snapshot not installed: present=%v digest=%q", ok, current.Digest)
	}
}

func TestStoreDoesNotExposeMutableLastKnownGoodState(t *testing.T) {
	store, _ := newWorkflowStore(t, validWorkflowSource)
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	loaded.Config.Tracker.Provider["owner"] = "mutated"
	loaded.Config.Tracker.RequiredLabels = append(loaded.Config.Tracker.RequiredLabels, "mutated")
	loaded.Definition.FrontMatter.Content[0].Content = nil

	current, ok := store.Current()
	if !ok {
		t.Fatal("last-known-good snapshot is missing")
	}
	if current.Config.Tracker.Provider["owner"] != "openai" || len(current.Definition.FrontMatter.Content[0].Content) == 0 {
		t.Fatal("caller mutation escaped into store-owned state")
	}
	current.Config.Tracker.Provider["owner"] = "mutated-again"
	again, _ := store.Current()
	if again.Config.Tracker.Provider["owner"] != "openai" {
		t.Fatal("Current returned a mutable alias of store-owned state")
	}
}

func TestInvalidReloadSafeErrorDoesNotExposeWorkflowContents(t *testing.T) {
	store, _ := newWorkflowStore(t, validWorkflowSource)
	canary := "DO-NOT-LEAK-RAW-PROMPT-CANARY"
	result := store.Validate(context.Background(), []byte("---\ntracker: ["+canary+"\n---\n"+canary))
	if result.Valid || len(result.GlobalErrors) == 0 {
		t.Fatalf("expected invalid safe result: %#v", result)
	}
	if strings.Contains(fmt.Sprintf("%+v", result), canary) {
		t.Fatal("safe validation result exposed workflow contents")
	}
}

func TestStoreProviderValidationUsesResolvedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(validWorkflowSource), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(context.Background(), path, os.LookupEnv, func(config EffectiveConfig) []FieldError {
		if config.Tracker.Kind == "github" {
			return []FieldError{{Field: "tracker.provider.owner", Code: "reserved", Message: "Choose another owner."}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	result := store.Validate(context.Background(), []byte(validWorkflowSource))
	if result.Valid || len(result.FieldErrors) != 1 || result.FieldErrors[0].Field != "tracker.provider.owner" {
		t.Fatalf("provider validation was not returned: valid=%v field-errors=%d", result.Valid, len(result.FieldErrors))
	}
}

func awaitChange(t *testing.T, changes <-chan Change) Change {
	t.Helper()
	select {
	case change, ok := <-changes:
		if !ok {
			t.Fatal("changes closed before expected event")
		}
		return change
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for workflow change")
		return Change{}
	}
}
