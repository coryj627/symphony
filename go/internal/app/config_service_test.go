package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

const validGitHubWorkflow = `---
tracker:
  kind: github
  provider:
    owner: coryj627
    repository: symphony
    credential_ref: os-vault
  required_labels: [symphony]
  active_states: [open]
  terminal_states: [closed]
workspace:
  root: .symphony/workspaces
server:
  port: 43127
---
Work on {{ issue.identifier }}: {{ issue.title }}.
`

const validLinearWorkflow = `---
tracker:
  kind: linear
  provider:
    project_slug: symphony-project
    credential_ref: os-vault
  required_labels: [symphony]
  active_states: [Todo, In Progress]
  terminal_states: [Done]
---
Work on {{ issue.identifier }}.
`

func TestViewReadsMissingAndInvalidCurrentBytesInsteadOfLastKnownGood(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "WORKFLOW.md")
	store := newTestWorkflowStore(t, ctx, path)
	service := NewConfigService(ConfigServiceOptions{Path: path, Store: store, Vault: &recordingVault{}, WorkflowID: "workflow-id", Platform: "darwin"})

	missing, err := service.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if missing.FileState != FileMissing || missing.Source != "" || missing.Digest != "" || missing.StructuredAvailable {
		t.Fatalf("missing view = %#v", missing)
	}

	if err := os.WriteFile(path, []byte(validGitHubWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	valid, err := service.View(ctx)
	if err != nil || valid.FileState != FileValid || !valid.StructuredAvailable {
		t.Fatalf("valid view = %#v, %v", valid, err)
	}

	invalidSource := "---\ntracker: []\n---\nsecret prompt canary"
	if err := os.WriteFile(path, []byte(invalidSource), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid, err := service.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(invalidSource)))
	if invalid.FileState != FileInvalid || invalid.Source != invalidSource || invalid.Digest != wantDigest || invalid.StructuredAvailable {
		t.Fatalf("invalid view did not expose exact current source safely: %#v", invalid)
	}
	if invalid.Config.TrackerKind != "" {
		t.Fatal("invalid view reused last-known-good structured values")
	}
	for _, problem := range append(invalid.Validation.FieldErrors, fieldErrorsFromGlobal(invalid.Validation.GlobalErrors)...) {
		if strings.Contains(problem.Message, "secret prompt canary") || strings.Contains(problem.Message, directory) {
			t.Fatal("safe validation exposed prompt or filesystem internals")
		}
	}
}

func TestTrackerValidationAndScopeCoverGitHubAndLinear(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		tracker    workflow.TrackerConfig
		wantScope  string
		wantKind   string
		wantErrors int
	}{
		{
			name: "github",
			tracker: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj627", "repository": "symphony", "credential_ref": "os-vault",
			}},
			wantScope: "github:coryj627/symphony", wantKind: "github",
		},
		{
			name: "linear",
			tracker: workflow.TrackerConfig{Kind: "linear", Provider: map[string]any{
				"project_slug": "symphony-project", "credential_ref": "os-vault",
			}},
			wantScope: "linear:symphony-project", wantKind: "linear",
		},
		{
			name: "typed field error",
			tracker: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"repository": "symphony",
			}},
			wantErrors: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := workflow.EffectiveConfig{Tracker: test.tracker}
			errors := ValidateTracker(config)
			if len(errors) != test.wantErrors {
				t.Fatalf("ValidateTracker() = %#v", errors)
			}
			if test.wantErrors > 0 {
				if errors[0].Field != "tracker.provider.owner" || errors[0].Code != "invalid_tracker_config" {
					t.Fatalf("translated error = %#v", errors[0])
				}
				return
			}
			provider, err := tracker.DecodeConfig(test.tracker)
			if err != nil {
				t.Fatal(err)
			}
			selection, err := SelectTracker(provider)
			if err != nil {
				t.Fatal(err)
			}
			if selection.Scope != test.wantScope || selection.Kind != test.wantKind {
				t.Fatalf("SelectTracker() = %#v", selection)
			}
		})
	}
}

func TestSaveRawPreservesExactBytesAndReportsRestartUsingRequestedPort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(validGitHubWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newTestWorkflowStore(t, ctx, path)
	service := NewConfigService(ConfigServiceOptions{
		Path: path, Store: store, Vault: &recordingVault{}, WorkflowID: "workflow-id", Platform: "darwin",
		RequestedPort: 43127,
	})
	view, err := service.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(validGitHubWorkflow, "port: 43127", "port: 0", 1) + "\n"
	result, err := service.Save(ctx, workflow.SaveCommand{BaseDigest: view.Digest, RawSource: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Source != raw || !result.RestartRequired {
		t.Fatalf("save result = %#v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != raw {
		t.Fatalf("saved bytes = %q, %v", got, err)
	}

	overridden := NewConfigService(ConfigServiceOptions{
		Path: path, Store: store, Vault: &recordingVault{}, WorkflowID: "workflow-id", Platform: "darwin",
		RequestedPort: 0, PortOverride: true,
	})
	second, err := overridden.View(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rawWithNewFilePort := strings.Replace(raw, "port: 0", "port: 65535", 1)
	result, err = overridden.Save(ctx, workflow.SaveCommand{BaseDigest: second.Digest, RawSource: []byte(rawWithNewFilePort)})
	if err != nil || result.RestartRequired {
		t.Fatalf("CLI-overridden save result = %#v, %v", result, err)
	}
}

func TestSaveTreatsDurabilityUncertainAsCompletedWithWarning(t *testing.T) {
	t.Parallel()
	snapshot := workflow.Snapshot{Source: validGitHubWorkflow, Config: workflow.EffectiveConfig{Server: workflow.ServerConfig{Port: 43127}}}
	store := &stubWorkflowStore{saveSnapshot: snapshot, saveErr: workflow.ErrDurabilityUncertain}
	service := NewConfigService(ConfigServiceOptions{Path: "WORKFLOW.md", Store: store, Vault: &recordingVault{}, WorkflowID: "workflow-id"})

	result, err := service.Save(context.Background(), workflow.SaveCommand{RawSource: []byte(validGitHubWorkflow)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Warning.Code != "workflow_durability_uncertain" || result.Snapshot.Source == "" {
		t.Fatalf("completed durability result = %#v", result)
	}
}

func TestCredentialStatesNeverReadOrExposeValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		source    string
		platform  string
		status    secrets.Status
		wantLabel string
		managed   bool
	}{
		{name: "macOS stored", source: validGitHubWorkflow, platform: "darwin", status: secrets.Status{Present: true, Backend: "native-keyring"}, wantLabel: "Stored in macOS Keychain"},
		{name: "Windows stored", source: validGitHubWorkflow, platform: "windows", status: secrets.Status{Present: true, Backend: "native-keyring"}, wantLabel: "Stored in Windows Credential Manager"},
		{name: "not configured", source: validGitHubWorkflow, platform: "darwin", status: secrets.Status{ErrorCode: "not_found"}, wantLabel: "Not configured"},
		{name: "environment managed", source: strings.Replace(validGitHubWorkflow, "credential_ref: os-vault", "credential_ref: $GH_TOKEN", 1), platform: "darwin", wantLabel: "Environment managed", managed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "WORKFLOW.md")
			if err := os.WriteFile(path, []byte(test.source), 0o600); err != nil {
				t.Fatal(err)
			}
			vault := &recordingVault{status: test.status}
			store := newTestWorkflowStore(t, ctx, path)
			service := NewConfigService(ConfigServiceOptions{Path: path, Store: store, Vault: vault, WorkflowID: "stable-workflow-id", Platform: test.platform})
			state, err := service.CredentialStatus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if state.Label != test.wantLabel || state.EnvironmentManaged != test.managed {
				t.Fatalf("state = %#v", state)
			}
			if vault.getCalls != 0 {
				t.Fatal("credential status called Get")
			}
			if !test.managed && vault.lastRef != (secrets.Ref{WorkflowID: "stable-workflow-id", TrackerKind: "github"}) {
				t.Fatalf("credential ref = %#v", vault.lastRef)
			}
		})
	}
}

func TestCredentialReplaceClearsCallerBufferAndDeleteUsesCurrentKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(validLinearWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	vault := &recordingVault{}
	service := NewConfigService(ConfigServiceOptions{
		Path: path, Store: newTestWorkflowStore(t, ctx, path), Vault: vault, WorkflowID: "stable-workflow-id", Platform: "darwin",
	})
	credential := []byte("credential-canary")
	binding := CredentialBinding{TrackerKind: "linear", BaseDigest: sourceDigest([]byte(validLinearWorkflow))}
	if err := service.ReplaceCredential(ctx, binding, credential); err != nil {
		t.Fatal(err)
	}
	if strings.Trim(string(credential), "\x00") != "" {
		t.Fatalf("caller credential buffer was not cleared: %v", credential)
	}
	if string(vault.putCopy) != "credential-canary" || vault.lastRef.TrackerKind != "linear" || vault.lastRef.WorkflowID != "stable-workflow-id" {
		t.Fatalf("vault put = %q at %#v", vault.putCopy, vault.lastRef)
	}
	if err := service.DeleteCredential(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if vault.deleteCalls != 1 || vault.lastRef.TrackerKind != "linear" {
		t.Fatalf("vault delete calls/ref = %d/%#v", vault.deleteCalls, vault.lastRef)
	}
	vault.deleteErr = secrets.ErrNotFound
	if err := service.DeleteCredential(ctx, binding); err != nil {
		t.Fatalf("not-found delete should be safe: %v", err)
	}
}

func TestCredentialMutationsRejectChangedDisplayedTrackerBinding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		initial  string
		external string
		kind     string
		mutate   func(context.Context, *ConfigService, CredentialBinding) error
	}{
		{
			name: "replace GitHub page after Linear switch", initial: validGitHubWorkflow, external: validLinearWorkflow, kind: "github",
			mutate: func(ctx context.Context, service *ConfigService, binding CredentialBinding) error {
				return service.ReplaceCredential(ctx, binding, []byte("must-not-be-stored"))
			},
		},
		{
			name: "delete Linear page after GitHub switch", initial: validLinearWorkflow, external: validGitHubWorkflow, kind: "linear",
			mutate: func(ctx context.Context, service *ConfigService, binding CredentialBinding) error {
				return service.DeleteCredential(ctx, binding)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "WORKFLOW.md")
			if err := os.WriteFile(path, []byte(test.initial), 0o600); err != nil {
				t.Fatal(err)
			}
			vault := &recordingVault{}
			service := NewConfigService(ConfigServiceOptions{
				Path: path, Store: newTestWorkflowStore(t, ctx, path), Vault: vault, WorkflowID: "stable-workflow-id",
			})
			if err := os.WriteFile(path, []byte(test.external), 0o600); err != nil {
				t.Fatal(err)
			}
			binding := CredentialBinding{TrackerKind: test.kind, BaseDigest: sourceDigest([]byte(test.initial))}
			if err := test.mutate(ctx, service, binding); !errors.Is(err, ErrCredentialConflict) {
				t.Fatalf("mutation error = %v, want credential conflict", err)
			}
			if vault.putCalls != 0 || vault.deleteCalls != 0 {
				t.Fatalf("stale binding touched vault: put %d delete %d", vault.putCalls, vault.deleteCalls)
			}
		})
	}
}

func TestEnvironmentManagedCredentialRejectsVaultMutationAndStillClearsBuffer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	source := strings.Replace(validGitHubWorkflow, "credential_ref: os-vault", "credential_ref: $GH_TOKEN", 1)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	vault := &recordingVault{}
	service := NewConfigService(ConfigServiceOptions{Path: path, Store: newTestWorkflowStore(t, ctx, path), Vault: vault, WorkflowID: "workflow-id"})
	credential := []byte("environment-canary")
	binding := CredentialBinding{TrackerKind: "github", BaseDigest: sourceDigest([]byte(source))}
	if !errors.Is(service.ReplaceCredential(ctx, binding, credential), ErrEnvironmentManagedCredential) {
		t.Fatal("environment-managed replace did not fail safely")
	}
	if strings.Trim(string(credential), "\x00") != "" || vault.putCalls != 0 {
		t.Fatal("environment-managed replace retained bytes or touched vault")
	}
	if !errors.Is(service.DeleteCredential(ctx, binding), ErrEnvironmentManagedCredential) || vault.deleteCalls != 0 {
		t.Fatal("environment-managed delete touched vault")
	}
}

func newTestWorkflowStore(t *testing.T, ctx context.Context, path string) *workflow.FileStore {
	t.Helper()
	store, err := workflow.NewStore(ctx, path, func(string) (string, bool) { return "", false }, ValidateTracker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close workflow store: %v", err)
		}
	})
	return store
}

func fieldErrorsFromGlobal(global []workflow.SafeError) []workflow.FieldError {
	result := make([]workflow.FieldError, len(global))
	for index, problem := range global {
		result[index] = workflow.FieldError{Code: problem.Code, Message: problem.Message}
	}
	return result
}

type recordingVault struct {
	status      secrets.Status
	lastRef     secrets.Ref
	putCopy     []byte
	putCalls    int
	getCalls    int
	deleteCalls int
	putErr      error
	deleteErr   error
}

func (vault *recordingVault) Put(_ context.Context, ref secrets.Ref, value []byte) error {
	vault.lastRef = ref
	vault.putCopy = append([]byte(nil), value...)
	vault.putCalls++
	return vault.putErr
}

func (vault *recordingVault) Get(context.Context, secrets.Ref) ([]byte, error) {
	vault.getCalls++
	return []byte("must-never-be-read"), nil
}

func (vault *recordingVault) Delete(_ context.Context, ref secrets.Ref) error {
	vault.lastRef = ref
	vault.deleteCalls++
	return vault.deleteErr
}

func (vault *recordingVault) Status(_ context.Context, ref secrets.Ref) secrets.Status {
	vault.lastRef = ref
	return vault.status
}

type stubWorkflowStore struct {
	saveSnapshot workflow.Snapshot
	saveErr      error
}

func (*stubWorkflowStore) Current() (workflow.Snapshot, bool) { return workflow.Snapshot{}, false }
func (*stubWorkflowStore) Load(context.Context) (workflow.Snapshot, error) {
	return workflow.Snapshot{}, errors.New("not implemented")
}
func (*stubWorkflowStore) Validate(context.Context, []byte) workflow.ValidationResult {
	return workflow.ValidationResult{Valid: true}
}
func (store *stubWorkflowStore) Save(context.Context, workflow.SaveCommand) (workflow.Snapshot, error) {
	return store.saveSnapshot, store.saveErr
}
func (*stubWorkflowStore) Changes() <-chan workflow.Change { return nil }
