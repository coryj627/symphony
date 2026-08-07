package tracker

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestDecodeGitHubConfig(t *testing.T) {
	// Break caught: losing the profile values or provider defaults makes a
	// repository profile select the wrong scope or credential contract.
	raw := workflow.TrackerConfig{
		Kind: "github",
		Provider: map[string]any{
			"owner": "coryj627", "repository": "symphony",
			"endpoint": "https://api.github.com", "credential_ref": "os-vault",
			"assignee": "coryj627",
		},
		ActiveStates: []string{"open"}, TerminalStates: []string{"closed"},
	}
	cfg, err := DecodeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cfg.(GitHubConfig)
	if !ok {
		t.Fatalf("config type = %T, want GitHubConfig", cfg)
	}
	if got.Owner != "coryj627" || got.Repository != "symphony" || got.CredentialRef != "os-vault" {
		t.Fatalf("unexpected config: %#v", got)
	}
	if got.Endpoint != "https://api.github.com" || got.CredentialEnv != "GITHUB_TOKEN" {
		t.Fatalf("defaults = %#v", got)
	}
	if got.Kind() != "github" || got.Credential() != (CredentialSpec{Reference: "os-vault", EnvName: "GITHUB_TOKEN"}) {
		t.Fatalf("credential contract = kind %q credential %#v", got.Kind(), got.Credential())
	}
	if names := got.SecretEnvironmentNames(); len(names) != 2 || names[0] != "GH_TOKEN" || names[1] != "GITHUB_TOKEN" {
		t.Fatalf("secret environment names = %#v", names)
	}
}

func TestDecodeLinearConfigDefaultsProfile(t *testing.T) {
	// Break caught: missing adapter defaults leaves the scheduler without a
	// usable Linear endpoint or its provider-native state categories.
	cfg, err := DecodeConfig(workflow.TrackerConfig{
		Kind:     "linear",
		Provider: map[string]any{"project_slug": "symphony"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cfg.(LinearConfig)
	if !ok {
		t.Fatalf("config type = %T, want LinearConfig", cfg)
	}
	if got.ProjectSlug != "symphony" || got.Endpoint != "https://api.linear.app/graphql" || got.CredentialEnv != "LINEAR_API_KEY" {
		t.Fatalf("defaults = %#v", got)
	}
	if got.Kind() != "linear" || got.Credential() != (CredentialSpec{EnvName: "LINEAR_API_KEY"}) {
		t.Fatalf("credential contract = kind %q credential %#v", got.Kind(), got.Credential())
	}
	if names := got.SecretEnvironmentNames(); len(names) != 1 || names[0] != "LINEAR_API_KEY" {
		t.Fatalf("secret environment names = %#v", names)
	}
}

func TestDecodeConfigMaterializesProviderStateDefaults(t *testing.T) {
	// Break caught: returning empty state lists makes the scheduler have no
	// eligible or terminal states when a valid profile relies on its defaults.
	for _, test := range []struct {
		name         string
		raw          workflow.TrackerConfig
		wantActive   []string
		wantTerminal []string
	}{
		{
			name: "GitHub defaults",
			raw: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj627", "repository": "symphony",
			}},
			wantActive:   []string{"open"},
			wantTerminal: []string{"closed"},
		},
		{
			name: "Linear defaults",
			raw: workflow.TrackerConfig{Kind: "linear", Provider: map[string]any{
				"project_slug": "symphony",
			}},
			wantActive:   []string{"Todo", "In Progress"},
			wantTerminal: []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := DecodeConfig(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			active, terminal := decodedProfileStates(t, config)
			if !reflect.DeepEqual(active, test.wantActive) || !reflect.DeepEqual(terminal, test.wantTerminal) {
				t.Fatalf("states = active %#v terminal %#v, want active %#v terminal %#v", active, terminal, test.wantActive, test.wantTerminal)
			}
		})
	}
}

func TestDecodeConfigPreservesAuthoredProviderStateOverrides(t *testing.T) {
	// Break caught: replacing non-empty authored state lists with profile
	// defaults prevents users from selecting their intended Linear workflow or
	// preserving a valid GitHub state list exactly.
	for _, test := range []struct {
		name         string
		raw          workflow.TrackerConfig
		wantActive   []string
		wantTerminal []string
	}{
		{
			name: "GitHub authored states",
			raw: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj627", "repository": "symphony",
			}, ActiveStates: []string{"open", "open"}, TerminalStates: []string{"closed", "closed"}},
			wantActive:   []string{"open", "open"},
			wantTerminal: []string{"closed", "closed"},
		},
		{
			name: "Linear authored states",
			raw: workflow.TrackerConfig{Kind: "linear", Provider: map[string]any{
				"project_slug": "symphony",
			}, ActiveStates: []string{"Triage"}, TerminalStates: []string{"Archived"}},
			wantActive:   []string{"Triage"},
			wantTerminal: []string{"Archived"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := DecodeConfig(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			active, terminal := decodedProfileStates(t, config)
			if !reflect.DeepEqual(active, test.wantActive) || !reflect.DeepEqual(terminal, test.wantTerminal) {
				t.Fatalf("states = active %#v terminal %#v, want active %#v terminal %#v", active, terminal, test.wantActive, test.wantTerminal)
			}
		})
	}
}

func decodedProfileStates(t *testing.T, config ProviderConfig) ([]string, []string) {
	t.Helper()
	switch config := config.(type) {
	case GitHubConfig:
		return config.ActiveStates, config.TerminalStates
	case LinearConfig:
		return config.ActiveStates, config.TerminalStates
	default:
		t.Fatalf("config type = %T, want a provider profile", config)
		return nil, nil
	}
}

func TestDecodeConfigRejectsInvalidProfilesWithStableFieldPaths(t *testing.T) {
	// Break caught: accepting a profile that has no valid scope, authentication
	// reference, endpoint, label, or provider-native states leads to failed
	// dispatch; changing a field path leaves the configuration UI unable to
	// attach the error to the broken control.
	for _, test := range []struct {
		name  string
		raw   workflow.TrackerConfig
		field string
	}{
		{
			name:  "unsupported kind",
			raw:   workflow.TrackerConfig{Kind: "asana"},
			field: "tracker.kind",
		},
		{
			name: "GitHub endpoint must use HTTPS",
			raw: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj627", "repository": "symphony", "endpoint": "http://api.github.com",
			}},
			field: "tracker.provider.endpoint",
		},
		{
			name: "GitHub owner must be a valid repository owner",
			raw: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj/627", "repository": "symphony",
			}},
			field: "tracker.provider.owner",
		},
		{
			name: "GitHub repository must be a valid repository name",
			raw: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj627", "repository": "symphony/repo",
			}},
			field: "tracker.provider.repository",
		},
		{
			name: "GitHub active states are restricted to open",
			raw: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj627", "repository": "symphony",
			}, ActiveStates: []string{"in progress"}},
			field: "tracker.active_states",
		},
		{
			name: "GitHub terminal states are restricted to closed",
			raw: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj627", "repository": "symphony",
			}, TerminalStates: []string{"done"}},
			field: "tracker.terminal_states",
		},
		{
			name:  "Linear requires project slug",
			raw:   workflow.TrackerConfig{Kind: "linear", Provider: map[string]any{}},
			field: "tracker.provider.project_slug",
		},
		{
			name: "credential reference must be vault or environment variable",
			raw: workflow.TrackerConfig{Kind: "linear", Provider: map[string]any{
				"project_slug": "symphony", "credential_ref": "literal-token",
			}},
			field: "tracker.provider.credential_ref",
		},
		{
			name: "required labels cannot be blank",
			raw: workflow.TrackerConfig{Kind: "linear", Provider: map[string]any{
				"project_slug": "symphony",
			}, RequiredLabels: []string{"  "}},
			field: "tracker.required_labels",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeConfig(test.raw)
			if !errors.Is(err, ErrInvalidTrackerConfig) {
				t.Fatalf("error = %v, want ErrInvalidTrackerConfig", err)
			}
			if !strings.HasPrefix(err.Error(), test.field) {
				t.Fatalf("error = %q, want stable field path %q", err, test.field)
			}
			var configError *ConfigError
			if !errors.As(err, &configError) || configError.Field != test.field {
				t.Fatalf("config error = %#v, want field %q", configError, test.field)
			}
		})
	}
}

func TestDecodeConfigAcceptsEnvironmentCredentialReference(t *testing.T) {
	// Break caught: rejecting a documented environment reference prevents a
	// previously-authored workflow from running without rewriting credentials.
	cfg, err := DecodeConfig(workflow.TrackerConfig{
		Kind: "github",
		Provider: map[string]any{
			"owner": "coryj627", "repository": "symphony", "credential_ref": "$SYMPHONY_GITHUB_TOKEN",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.(GitHubConfig)
	if got.CredentialRef != "$SYMPHONY_GITHUB_TOKEN" || got.CredentialEnv != "SYMPHONY_GITHUB_TOKEN" {
		t.Fatalf("environment credential = %#v", got)
	}
	if got.Credential() != (CredentialSpec{Reference: "$SYMPHONY_GITHUB_TOKEN", EnvName: "SYMPHONY_GITHUB_TOKEN"}) {
		t.Fatalf("credential = %#v", got.Credential())
	}
	if names := got.SecretEnvironmentNames(); len(names) != 3 || names[0] != "GH_TOKEN" || names[1] != "GITHUB_TOKEN" || names[2] != "SYMPHONY_GITHUB_TOKEN" {
		t.Fatalf("secret environment names = %#v", names)
	}
}
