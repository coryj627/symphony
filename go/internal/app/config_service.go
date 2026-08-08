package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type FileState string

const (
	FileMissing FileState = "missing"
	FileInvalid FileState = "invalid"
	FileValid   FileState = "valid"
)

var (
	ErrCredentialUnavailable        = errors.New("credential_unavailable")
	ErrCredentialRequired           = errors.New("credential_required")
	ErrEnvironmentManagedCredential = errors.New("environment_managed_credential")
)

type ConfigServiceOptions struct {
	Path          string
	Store         workflow.Store
	Vault         secrets.Store
	WorkflowID    string
	Platform      string
	RequestedPort int
	PortOverride  bool
}

type ConfigService struct {
	path          string
	store         workflow.Store
	vault         secrets.Store
	workflowID    string
	platform      string
	requestedPort int
	portOverride  bool
}

type TrackerSelection struct {
	Kind                string
	Scope               string
	CredentialReference string
	CredentialEnv       string
}

type ConfigValues struct {
	TrackerKind         string
	ProviderOwner       string
	ProviderRepository  string
	ProviderProjectSlug string
	ProviderEndpoint    string
	CredentialRef       string
	ProviderAssignee    string
	RequiredLabels      string
	ActiveStates        string
	TerminalStates      string

	PollingIntervalMS string
	WorkspaceRoot     string

	HookAfterCreate  string
	HookBeforeRun    string
	HookAfterRun     string
	HookBeforeRemove string
	HookTimeoutMS    string

	AgentMaxConcurrent     string
	AgentMaxTurns          string
	AgentMaxRetryBackoffMS string

	CodexCommand        string
	CodexApprovalPolicy string
	CodexThreadSandbox  string
	CodexTurnTimeoutMS  string
	CodexReadTimeoutMS  string
	CodexStallTimeoutMS string

	ServerPort                      string
	ServerOperatorResponseTimeoutMS string
}

type ConfigView struct {
	FileState           FileState
	Source              string
	Digest              string
	StructuredAvailable bool
	Validation          workflow.ValidationResult
	Config              ConfigValues
	Tracker             TrackerSelection
	PortOverride        bool
	RequestedPort       int
}

type SaveResult struct {
	Snapshot        workflow.Snapshot
	Warning         workflow.SafeError
	RestartRequired bool
}

type CredentialState struct {
	Label              string
	Present            bool
	EnvironmentManaged bool
	TrackerKind        string
}

func NewConfigService(options ConfigServiceOptions) *ConfigService {
	platform := options.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	return &ConfigService{
		path: options.Path, store: options.Store, vault: options.Vault,
		workflowID: options.WorkflowID, platform: platform,
		requestedPort: options.RequestedPort, portOverride: options.PortOverride,
	}
}

// ValidateTracker is the one safe translator from provider validation into
// workflow field errors used by startup, saves, and UI validation.
func ValidateTracker(config workflow.EffectiveConfig) []workflow.FieldError {
	_, err := tracker.DecodeConfig(config.Tracker)
	if err == nil {
		return nil
	}
	var configError *tracker.ConfigError
	if errors.As(err, &configError) {
		return []workflow.FieldError{{
			Field: configError.Field, Code: "invalid_tracker_config", Message: configError.Detail,
		}}
	}
	return []workflow.FieldError{{
		Field: "tracker.kind", Code: "invalid_tracker_config", Message: "Tracker configuration is invalid.",
	}}
}

// SelectTracker centralizes provider-specific scope and credential encoding.
func SelectTracker(provider tracker.ProviderConfig) (TrackerSelection, error) {
	credential := provider.Credential()
	switch config := provider.(type) {
	case tracker.GitHubConfig:
		return TrackerSelection{
			Kind: "github", Scope: "github:" + config.Owner + "/" + config.Repository,
			CredentialReference: credential.Reference, CredentialEnv: credential.EnvName,
		}, nil
	case tracker.LinearConfig:
		return TrackerSelection{
			Kind: "linear", Scope: "linear:" + config.ProjectSlug,
			CredentialReference: credential.Reference, CredentialEnv: credential.EnvName,
		}, nil
	default:
		return TrackerSelection{}, errors.New("unsupported_tracker_kind")
	}
}

func (service *ConfigService) View(ctx context.Context) (ConfigView, error) {
	if service == nil || service.store == nil || service.path == "" {
		return ConfigView{}, errors.New("configuration_service_unavailable")
	}
	for attempts := 0; attempts < 8; attempts++ {
		source, err := os.ReadFile(service.path)
		if errors.Is(err, os.ErrNotExist) {
			return ConfigView{
				FileState: FileMissing, Validation: workflow.ValidationResult{
					Valid: false, FieldErrors: []workflow.FieldError{},
					GlobalErrors: []workflow.SafeError{{Code: "missing_workflow_file", Message: "The workflow file does not exist. Create it with the complete workflow editor."}},
				},
				PortOverride: service.portOverride, RequestedPort: service.requestedPort,
			}, nil
		}
		if err != nil {
			return ConfigView{}, errors.New("workflow_read_failed")
		}
		digest := sourceDigest(source)
		validation := service.store.Validate(ctx, source)
		if !validation.Valid {
			return ConfigView{
				FileState: FileInvalid, Source: string(source), Digest: digest,
				Validation: validation, PortOverride: service.portOverride, RequestedPort: service.requestedPort,
			}, nil
		}
		snapshot, err := service.store.Load(ctx)
		if err != nil {
			if errors.Is(err, workflow.ErrInvalidWorkflow) || errors.Is(err, workflow.ErrSaveConflict) {
				continue
			}
			return ConfigView{}, errors.New("workflow_load_failed")
		}
		if snapshot.Digest != digest {
			continue
		}
		provider, err := tracker.DecodeConfig(snapshot.Config.Tracker)
		if err != nil {
			return ConfigView{
				FileState: FileInvalid, Source: string(source), Digest: digest,
				Validation:   workflow.ValidationResult{Valid: false, FieldErrors: ValidateTracker(snapshot.Config), GlobalErrors: []workflow.SafeError{}},
				PortOverride: service.portOverride, RequestedPort: service.requestedPort,
			}, nil
		}
		selection, err := SelectTracker(provider)
		if err != nil {
			return ConfigView{}, errors.New("tracker_selection_failed")
		}
		return ConfigView{
			FileState: FileValid, Source: string(source), Digest: digest,
			StructuredAvailable: true, Validation: validation,
			Config: configValues(snapshot.Config), Tracker: selection,
			PortOverride: service.portOverride, RequestedPort: service.requestedPort,
		}, nil
	}
	return ConfigView{}, workflow.ErrSaveConflict
}

func (service *ConfigService) Validate(ctx context.Context, source []byte) workflow.ValidationResult {
	if service == nil || service.store == nil {
		return workflow.ValidationResult{Valid: false, GlobalErrors: []workflow.SafeError{{Code: "validation_unavailable", Message: "Workflow validation is unavailable."}}, FieldErrors: []workflow.FieldError{}}
	}
	return service.store.Validate(ctx, source)
}

func (service *ConfigService) Save(ctx context.Context, command workflow.SaveCommand) (SaveResult, error) {
	snapshot, err := service.store.Save(ctx, command)
	if errors.Is(err, workflow.ErrSaveConflict) {
		return SaveResult{}, err
	}
	if err != nil && !errors.Is(err, workflow.ErrDurabilityUncertain) {
		return SaveResult{}, err
	}
	result := SaveResult{Snapshot: snapshot}
	if !service.portOverride {
		result.RestartRequired = snapshot.Config.Server.Port != service.requestedPort
	}
	if errors.Is(err, workflow.ErrDurabilityUncertain) {
		result.Warning = workflow.SafeError{
			Code:    "workflow_durability_uncertain",
			Message: "Configuration was saved, but the operating system could not confirm directory durability. Review the saved workflow before restarting.",
		}
	}
	return result, nil
}

func (service *ConfigService) CredentialStatus(ctx context.Context) (CredentialState, error) {
	selection, err := service.currentTracker(ctx)
	if err != nil {
		return CredentialState{}, err
	}
	state := CredentialState{Label: "Not configured", TrackerKind: selection.Kind}
	if isEnvironmentReference(selection.CredentialReference) {
		state.Label = "Environment managed"
		state.EnvironmentManaged = true
		return state, nil
	}
	if service.vault == nil {
		return state, nil
	}
	status := service.vault.Status(ctx, service.credentialRef(selection.Kind))
	if !status.Present {
		return state, nil
	}
	state.Present = true
	if service.platform == "windows" {
		state.Label = "Stored in Windows Credential Manager"
	} else {
		state.Label = "Stored in macOS Keychain"
	}
	return state, nil
}

func (service *ConfigService) ReplaceCredential(ctx context.Context, value []byte) error {
	defer clear(value)
	selection, err := service.currentTracker(ctx)
	if err != nil {
		return err
	}
	if isEnvironmentReference(selection.CredentialReference) {
		return ErrEnvironmentManagedCredential
	}
	if len(value) == 0 {
		return ErrCredentialRequired
	}
	if service.vault == nil {
		return ErrCredentialUnavailable
	}
	return service.vault.Put(ctx, service.credentialRef(selection.Kind), value)
}

func (service *ConfigService) DeleteCredential(ctx context.Context) error {
	selection, err := service.currentTracker(ctx)
	if err != nil {
		return err
	}
	if isEnvironmentReference(selection.CredentialReference) {
		return ErrEnvironmentManagedCredential
	}
	if service.vault == nil {
		return ErrCredentialUnavailable
	}
	err = service.vault.Delete(ctx, service.credentialRef(selection.Kind))
	if errors.Is(err, secrets.ErrNotFound) {
		return nil
	}
	return err
}

func (service *ConfigService) currentTracker(ctx context.Context) (TrackerSelection, error) {
	view, err := service.View(ctx)
	if err != nil {
		return TrackerSelection{}, err
	}
	if view.FileState != FileValid || view.Tracker.Kind == "" {
		return TrackerSelection{}, ErrCredentialUnavailable
	}
	return view.Tracker, nil
}

func (service *ConfigService) credentialRef(kind string) secrets.Ref {
	return secrets.Ref{WorkflowID: service.workflowID, TrackerKind: kind}
}

func isEnvironmentReference(reference string) bool {
	return strings.HasPrefix(reference, "$") && len(reference) > 1
}

func sourceDigest(source []byte) string {
	digest := sha256.Sum256(source)
	return fmt.Sprintf("%x", digest)
}

func configValues(config workflow.EffectiveConfig) ConfigValues {
	values := ConfigValues{
		TrackerKind:       config.Tracker.Kind,
		RequiredLabels:    strings.Join(config.Tracker.RequiredLabels, "\n"),
		ActiveStates:      strings.Join(config.Tracker.ActiveStates, "\n"),
		TerminalStates:    strings.Join(config.Tracker.TerminalStates, "\n"),
		PollingIntervalMS: durationMilliseconds(config.Polling.Interval),
		WorkspaceRoot:     config.Workspace.Root,
		HookAfterCreate:   config.Hooks.AfterCreate, HookBeforeRun: config.Hooks.BeforeRun,
		HookAfterRun: config.Hooks.AfterRun, HookBeforeRemove: config.Hooks.BeforeRemove,
		HookTimeoutMS:                   durationMilliseconds(config.Hooks.Timeout),
		AgentMaxConcurrent:              strconv.Itoa(config.Agent.MaxConcurrent),
		AgentMaxTurns:                   strconv.Itoa(config.Agent.MaxTurns),
		AgentMaxRetryBackoffMS:          durationMilliseconds(config.Agent.MaxRetryBackoff),
		CodexCommand:                    config.Codex.Command,
		CodexApprovalPolicy:             printableValue(config.Codex.ApprovalPolicy),
		CodexThreadSandbox:              config.Codex.ThreadSandbox,
		CodexTurnTimeoutMS:              durationMilliseconds(config.Codex.TurnTimeout),
		CodexReadTimeoutMS:              durationMilliseconds(config.Codex.ReadTimeout),
		CodexStallTimeoutMS:             durationMilliseconds(config.Codex.StallTimeout),
		ServerPort:                      strconv.Itoa(config.Server.Port),
		ServerOperatorResponseTimeoutMS: durationMilliseconds(config.Server.OperatorResponseWindow),
	}
	if value, ok := config.Tracker.Provider["owner"].(string); ok {
		values.ProviderOwner = value
	}
	if value, ok := config.Tracker.Provider["repository"].(string); ok {
		values.ProviderRepository = value
	}
	if value, ok := config.Tracker.Provider["project_slug"].(string); ok {
		values.ProviderProjectSlug = value
	}
	if value, ok := config.Tracker.Provider["endpoint"].(string); ok {
		values.ProviderEndpoint = value
	}
	if value, ok := config.Tracker.Provider["credential_ref"].(string); ok {
		values.CredentialRef = value
	}
	if value, ok := config.Tracker.Provider["assignee"].(string); ok {
		values.ProviderAssignee = value
	}
	return values
}

func durationMilliseconds(value interface{ Milliseconds() int64 }) string {
	return strconv.FormatInt(value.Milliseconds(), 10)
}

func printableValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}
