package tracker

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/coryj627/symphony/go/internal/workflow"
)

const (
	gitHubEndpoint       = "https://api.github.com"
	gitHubCredentialEnv  = "GITHUB_TOKEN"
	linearEndpoint       = "https://api.linear.app/graphql"
	linearCredentialEnv  = "LINEAR_API_KEY"
	credentialRefOSVault = "os-vault"
)

var (
	gitHubOwnerPattern     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	gitHubRepoPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	environmentPattern     = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	gitHubActiveDefaults   = []string{"open"}
	gitHubTerminalDefaults = []string{"closed"}
	linearActiveDefaults   = []string{"Todo", "In Progress"}
	linearTerminalDefaults = []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
)

type CredentialSpec struct {
	Reference string
	EnvName   string
}

type ProviderConfig interface {
	Kind() string
	Credential() CredentialSpec
	SecretEnvironmentNames() []string
}

type GitHubConfig struct {
	Owner, Repository, Endpoint, CredentialRef, CredentialEnv, Assignee string
	ActiveStates, TerminalStates                                        []string
}

func (config GitHubConfig) Kind() string { return "github" }

func (config GitHubConfig) Credential() CredentialSpec {
	return CredentialSpec{Reference: config.CredentialRef, EnvName: config.CredentialEnv}
}

func (config GitHubConfig) SecretEnvironmentNames() []string {
	return secretEnvironmentNames([]string{"GH_TOKEN", gitHubCredentialEnv}, config.CredentialEnv)
}

type LinearConfig struct {
	ProjectSlug, Endpoint, CredentialRef, CredentialEnv string
	ActiveStates, TerminalStates                        []string
}

func (config LinearConfig) Kind() string { return "linear" }

func (config LinearConfig) Credential() CredentialSpec {
	return CredentialSpec{Reference: config.CredentialRef, EnvName: config.CredentialEnv}
}

func (config LinearConfig) SecretEnvironmentNames() []string {
	return secretEnvironmentNames([]string{linearCredentialEnv}, config.CredentialEnv)
}

func DecodeConfig(raw workflow.TrackerConfig) (ProviderConfig, error) {
	switch raw.Kind {
	case "github":
		return decodeGitHubConfig(raw)
	case "linear":
		return decodeLinearConfig(raw)
	default:
		return nil, invalidConfig("tracker.kind", "must be github or linear")
	}
}

func decodeGitHubConfig(raw workflow.TrackerConfig) (GitHubConfig, error) {
	owner, err := requiredProviderString(raw.Provider, "owner")
	if err != nil {
		return GitHubConfig{}, err
	}
	if !gitHubOwnerPattern.MatchString(owner) {
		return GitHubConfig{}, invalidConfig("tracker.provider.owner", "must be a valid GitHub owner")
	}
	repository, err := requiredProviderString(raw.Provider, "repository")
	if err != nil {
		return GitHubConfig{}, err
	}
	if !gitHubRepoPattern.MatchString(repository) {
		return GitHubConfig{}, invalidConfig("tracker.provider.repository", "must be a valid GitHub repository")
	}
	endpoint, err := providerEndpoint(raw.Provider, gitHubEndpoint)
	if err != nil {
		return GitHubConfig{}, err
	}
	credentialRef, credentialEnv, err := credential(raw.Provider, gitHubCredentialEnv)
	if err != nil {
		return GitHubConfig{}, err
	}
	assignee, err := optionalProviderString(raw.Provider, "assignee")
	if err != nil {
		return GitHubConfig{}, err
	}
	if err := validateRequiredLabels(raw.RequiredLabels); err != nil {
		return GitHubConfig{}, err
	}
	if err := validateGitHubStates(raw.ActiveStates, raw.TerminalStates); err != nil {
		return GitHubConfig{}, err
	}
	return GitHubConfig{
		Owner: owner, Repository: repository, Endpoint: endpoint,
		CredentialRef: credentialRef, CredentialEnv: credentialEnv, Assignee: assignee,
		ActiveStates: profileStates(raw.ActiveStates, gitHubActiveDefaults), TerminalStates: profileStates(raw.TerminalStates, gitHubTerminalDefaults),
	}, nil
}

func decodeLinearConfig(raw workflow.TrackerConfig) (LinearConfig, error) {
	projectSlug, err := requiredProviderString(raw.Provider, "project_slug")
	if err != nil {
		return LinearConfig{}, err
	}
	endpoint, err := providerEndpoint(raw.Provider, linearEndpoint)
	if err != nil {
		return LinearConfig{}, err
	}
	credentialRef, credentialEnv, err := credential(raw.Provider, linearCredentialEnv)
	if err != nil {
		return LinearConfig{}, err
	}
	if err := validateRequiredLabels(raw.RequiredLabels); err != nil {
		return LinearConfig{}, err
	}
	return LinearConfig{
		ProjectSlug: projectSlug, Endpoint: endpoint,
		CredentialRef: credentialRef, CredentialEnv: credentialEnv,
		ActiveStates: profileStates(raw.ActiveStates, linearActiveDefaults), TerminalStates: profileStates(raw.TerminalStates, linearTerminalDefaults),
	}, nil
}

func requiredProviderString(provider map[string]any, name string) (string, error) {
	value, err := optionalProviderString(provider, name)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", invalidConfig("tracker.provider."+name, "is required")
	}
	return value, nil
}

func optionalProviderString(provider map[string]any, name string) (string, error) {
	value, found := provider[name]
	if !found {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", invalidConfig("tracker.provider."+name, "must be a string")
	}
	return strings.TrimSpace(text), nil
}

func providerEndpoint(provider map[string]any, fallback string) (string, error) {
	endpoint, err := optionalProviderString(provider, "endpoint")
	if err != nil {
		return "", err
	}
	if endpoint == "" {
		return fallback, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", invalidConfig("tracker.provider.endpoint", "must be an HTTPS URL")
	}
	return endpoint, nil
}

func credential(provider map[string]any, fallbackEnvironment string) (string, string, error) {
	reference, err := optionalProviderString(provider, "credential_ref")
	if err != nil {
		return "", "", err
	}
	if reference == "" || reference == credentialRefOSVault {
		return reference, fallbackEnvironment, nil
	}
	matches := environmentPattern.FindStringSubmatch(reference)
	if matches == nil {
		return "", "", invalidConfig("tracker.provider.credential_ref", "must be os-vault or a $NAME environment reference")
	}
	return reference, matches[1], nil
}

func validateRequiredLabels(labels []string) error {
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			return invalidConfig("tracker.required_labels", "must not contain a blank label")
		}
	}
	return nil
}

func validateGitHubStates(active, terminal []string) error {
	for _, state := range active {
		if state != "open" {
			return invalidConfig("tracker.active_states", "must contain only open")
		}
	}
	for _, state := range terminal {
		if state != "closed" {
			return invalidConfig("tracker.terminal_states", "must contain only closed")
		}
	}
	return nil
}

func profileStates(authored, defaults []string) []string {
	if len(authored) > 0 {
		return append([]string(nil), authored...)
	}
	return append([]string(nil), defaults...)
}

func secretEnvironmentNames(defaults []string, explicit string) []string {
	seen := make(map[string]struct{}, len(defaults)+1)
	names := make([]string, 0, len(defaults)+1)
	for _, name := range append(defaults, explicit) {
		if _, found := seen[name]; found || name == "" {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

func invalidConfig(field, detail string) error {
	return &ConfigError{Field: field, Detail: detail}
}
