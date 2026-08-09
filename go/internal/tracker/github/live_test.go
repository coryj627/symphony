//go:build integration_live

package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

const (
	githubLiveEnableEnv = "SYMPHONY_RUN_GITHUB_LIVE"
	githubLiveScopeEnv  = "SYMPHONY_GITHUB_TEST_REPO"
	githubLiveTokenEnv  = "SYMPHONY_GITHUB_TEST_TOKEN"
)

type githubLiveProfile struct {
	enabled bool
	config  tracker.GitHubConfig
	token   []byte
}

type githubLiveLookup func(string) (string, bool)

func loadGitHubLiveProfile(lookup githubLiveLookup) (githubLiveProfile, error) {
	if lookup == nil {
		return githubLiveProfile{}, errors.New("GitHub live environment lookup is unavailable")
	}
	enable, found := lookup(githubLiveEnableEnv)
	if !found || enable == "0" {
		return githubLiveProfile{}, nil
	}
	if enable != "1" {
		return githubLiveProfile{}, errors.New("GitHub live enable flag must be 0 or 1")
	}
	scope, found := lookup(githubLiveScopeEnv)
	if !found || scope == "" {
		return githubLiveProfile{}, errors.New("GitHub live repository scope is required")
	}
	if strings.TrimSpace(scope) != scope || strings.Count(scope, "/") != 1 {
		return githubLiveProfile{}, errors.New("GitHub live repository scope must be owner/repository")
	}
	parts := strings.Split(scope, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.TrimSpace(parts[0]) != parts[0] || strings.TrimSpace(parts[1]) != parts[1] {
		return githubLiveProfile{}, errors.New("GitHub live repository scope must be owner/repository")
	}
	raw := workflow.TrackerConfig{
		Kind: "github",
		Provider: map[string]any{
			"owner": parts[0], "repository": parts[1], "credential_ref": "$" + githubLiveTokenEnv,
		},
		ActiveStates: []string{"open"}, TerminalStates: []string{"closed"},
	}
	decoded, err := tracker.DecodeConfig(raw)
	if err != nil {
		return githubLiveProfile{}, errors.New("GitHub live repository scope is invalid")
	}
	config, ok := decoded.(tracker.GitHubConfig)
	if !ok {
		return githubLiveProfile{}, errors.New("GitHub live tracker configuration is invalid")
	}
	tokenText, found := lookup(githubLiveTokenEnv)
	if !found || strings.TrimSpace(tokenText) == "" {
		return githubLiveProfile{}, errors.New("GitHub live token is required")
	}
	// Environment lookup exposes an immutable string that Go cannot reliably
	// erase. Copy it exactly once into memory owned and cleared by this test.
	ownedToken := []byte(tokenText)
	return githubLiveProfile{enabled: true, config: config, token: ownedToken}, nil
}

func TestGitHubLiveProfileConfiguration(t *testing.T) {
	const canary = "github-live-token-canary"
	for _, test := range []struct {
		name        string
		environment map[string]string
		enabled     bool
		wantError   bool
		wantLookups []string
	}{
		{name: "missing enable", environment: map[string]string{}, wantLookups: []string{githubLiveEnableEnv}},
		{name: "zero disabled", environment: map[string]string{githubLiveEnableEnv: "0", githubLiveTokenEnv: canary}, wantLookups: []string{githubLiveEnableEnv}},
		{name: "empty enable", environment: map[string]string{githubLiveEnableEnv: "", githubLiveTokenEnv: canary}, wantError: true, wantLookups: []string{githubLiveEnableEnv}},
		{name: "whitespace enable", environment: map[string]string{githubLiveEnableEnv: " 1 ", githubLiveTokenEnv: canary}, wantError: true, wantLookups: []string{githubLiveEnableEnv}},
		{name: "case enable", environment: map[string]string{githubLiveEnableEnv: "TRUE", githubLiveTokenEnv: canary}, wantError: true, wantLookups: []string{githubLiveEnableEnv}},
		{name: "numeric enable", environment: map[string]string{githubLiveEnableEnv: "01", githubLiveTokenEnv: canary}, wantError: true, wantLookups: []string{githubLiveEnableEnv}},
		{name: "missing scope", environment: map[string]string{githubLiveEnableEnv: "1", githubLiveTokenEnv: canary}, wantError: true, wantLookups: []string{githubLiveEnableEnv, githubLiveScopeEnv}},
		{name: "blank scope", environment: map[string]string{githubLiveEnableEnv: "1", githubLiveScopeEnv: " ", githubLiveTokenEnv: canary}, wantError: true, wantLookups: []string{githubLiveEnableEnv, githubLiveScopeEnv}},
		{name: "missing token", environment: map[string]string{githubLiveEnableEnv: "1", githubLiveScopeEnv: "coryj627/symphony"}, wantError: true, wantLookups: []string{githubLiveEnableEnv, githubLiveScopeEnv, githubLiveTokenEnv}},
		{name: "blank token", environment: map[string]string{githubLiveEnableEnv: "1", githubLiveScopeEnv: "coryj627/symphony", githubLiveTokenEnv: " "}, wantError: true, wantLookups: []string{githubLiveEnableEnv, githubLiveScopeEnv, githubLiveTokenEnv}},
		{name: "valid", environment: map[string]string{githubLiveEnableEnv: "1", githubLiveScopeEnv: "coryj627/symphony", githubLiveTokenEnv: canary}, enabled: true, wantLookups: []string{githubLiveEnableEnv, githubLiveScopeEnv, githubLiveTokenEnv}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups := []string{}
			profile, err := loadGitHubLiveProfile(func(name string) (string, bool) {
				lookups = append(lookups, name)
				value, found := test.environment[name]
				return value, found
			})
			defer zeroGitHubBytes(profile.token)
			if (err != nil) != test.wantError || profile.enabled != test.enabled {
				t.Fatalf("result enabled=%t error=%t", profile.enabled, err != nil)
			}
			if err != nil && strings.Contains(err.Error(), canary) {
				t.Fatal("configuration error exposed token canary")
			}
			if !reflect.DeepEqual(lookups, test.wantLookups) {
				t.Fatalf("lookups = %#v, want %#v", lookups, test.wantLookups)
			}
			if !profile.enabled && len(profile.token) != 0 {
				t.Fatal("disabled or invalid profile retained token bytes")
			}
			if profile.enabled {
				if string(profile.token) != canary || profile.config.Owner != "coryj627" || profile.config.Repository != "symphony" || profile.config.CredentialRef != "$"+githubLiveTokenEnv {
					t.Fatal("valid configuration did not preserve the selected profile")
				}
			}
		})
	}
}

func TestGitHubLiveProfileRejectsMalformedRepositoryScopeWithoutTokenLookup(t *testing.T) {
	for _, scope := range []string{
		"https://github.com/coryj627/symphony", "owner/repository/extra", "/repository", "owner/",
		" owner/repository", "owner /repository", "owner/repository ", "owner?query/repository",
	} {
		t.Run(strings.ReplaceAll(scope, "/", "_"), func(t *testing.T) {
			lookedUpToken := false
			_, err := loadGitHubLiveProfile(func(name string) (string, bool) {
				switch name {
				case githubLiveEnableEnv:
					return "1", true
				case githubLiveScopeEnv:
					return scope, true
				case githubLiveTokenEnv:
					lookedUpToken = true
					return "token-canary", true
				default:
					return "", false
				}
			})
			if err == nil || lookedUpToken || strings.Contains(err.Error(), "token-canary") {
				t.Fatalf("malformed scope was not rejected safely: error=%v token_lookup=%t", err, lookedUpToken)
			}
		})
	}
}

func TestGitHubLiveNativeRefRejectsInvalidRequiredAndOptionalValues(t *testing.T) {
	config := tracker.GitHubConfig{Owner: "coryj627", Repository: "symphony"}
	valid := githubLiveValidationIssue(config)
	valid.NativeRef["database_id"] = json.Number("9007199254740993")
	valid.NativeRef["node_id"] = "node-42"
	valid.NativeRef["state_reason"] = "reopened"
	if !validGitHubLiveNativeRef(valid, config) {
		t.Fatal("valid GitHub native reference was rejected")
	}
	for _, test := range []struct {
		name   string
		mutate func(*domain.Issue)
	}{
		{name: "zero issue number", mutate: func(issue *domain.Issue) { setGitHubLiveNumber(issue, config, "0") }},
		{name: "negative issue number", mutate: func(issue *domain.Issue) { setGitHubLiveNumber(issue, config, "-1") }},
		{name: "fractional issue number", mutate: func(issue *domain.Issue) { setGitHubLiveNumber(issue, config, "1.5") }},
		{name: "zero database ID", mutate: func(issue *domain.Issue) { issue.NativeRef["database_id"] = json.Number("0") }},
		{name: "negative database ID", mutate: func(issue *domain.Issue) { issue.NativeRef["database_id"] = json.Number("-1") }},
		{name: "fractional database ID", mutate: func(issue *domain.Issue) { issue.NativeRef["database_id"] = json.Number("1.5") }},
		{name: "database ID wrong type", mutate: func(issue *domain.Issue) { issue.NativeRef["database_id"] = "42" }},
		{name: "blank node ID", mutate: func(issue *domain.Issue) { issue.NativeRef["node_id"] = " " }},
		{name: "node ID wrong type", mutate: func(issue *domain.Issue) { issue.NativeRef["node_id"] = 42 }},
		{name: "unnormalized state reason", mutate: func(issue *domain.Issue) { issue.NativeRef["state_reason"] = " reopened " }},
		{name: "state reason wrong type", mutate: func(issue *domain.Issue) { issue.NativeRef["state_reason"] = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := githubLiveValidationIssue(config)
			test.mutate(&issue)
			if validGitHubLiveNativeRef(issue, config) {
				t.Fatal("malformed GitHub native reference was accepted")
			}
		})
	}
}

func githubLiveValidationIssue(config tracker.GitHubConfig) domain.Issue {
	return domain.Issue{
		ID: dispatchID(config, "42"), Identifier: "#42", Title: "Sentinel", State: "open",
		NativeRef: map[string]any{
			"owner": config.Owner, "repository": config.Repository, "number": json.Number("42"),
		},
		Labels: []string{"sentinel"}, BlockedBy: []domain.BlockerRef{},
	}
}

func setGitHubLiveNumber(issue *domain.Issue, config tracker.GitHubConfig, number string) {
	issue.NativeRef["number"] = json.Number(number)
	issue.ID = dispatchID(config, number)
	issue.Identifier = "#" + number
}

func TestGitHubLiveCollectionsRequireNormalizedLabelsAndExactlyEmptyBlockers(t *testing.T) {
	config := tracker.GitHubConfig{Owner: "coryj627", Repository: "symphony"}
	for _, test := range []struct {
		name      string
		blockedBy []domain.BlockerRef
		labels    []string
		wantValid bool
	}{
		{name: "normalized empty", blockedBy: []domain.BlockerRef{}, labels: []string{"sentinel"}, wantValid: true},
		{name: "nil blockers", blockedBy: nil, labels: []string{"sentinel"}},
		{name: "normalized nonempty blocker", blockedBy: []domain.BlockerRef{{ID: stringPointer("blocker-1")}}, labels: []string{"sentinel"}},
		{name: "blank blocker", blockedBy: []domain.BlockerRef{{ID: stringPointer(" ")}}, labels: []string{"sentinel"}},
		{name: "unnormalized labels", blockedBy: []domain.BlockerRef{}, labels: []string{" Sentinel "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := githubLiveValidationIssue(config)
			issue.BlockedBy = test.blockedBy
			issue.Labels = test.labels
			if got := validGitHubLiveCollections(issue); got != test.wantValid {
				t.Fatalf("collection validity = %t, want %t", got, test.wantValid)
			}
		})
	}
}

func TestGitHubLiveProfile(t *testing.T) {
	profile, err := loadGitHubLiveProfile(os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.enabled {
		t.Skip("SKIPPED: GitHub live profile not enabled")
	}
	defer zeroGitHubBytes(profile.token)
	if err := os.Unsetenv(githubLiveTokenEnv); err != nil {
		t.Fatal("GitHub live token could not be removed from the environment")
	}
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret(profile.token)
	logger := slog.New(githubLiveRedactingHandler{redactor: redactor, delegate: slog.NewJSONHandler(io.Discard, nil)})

	adapter, err := New(profile.config, profile.token, nil, logger)
	if err != nil {
		t.Fatal("GitHub live adapter could not be created")
	}
	defer zeroGitHubBytes(adapter.token)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	issues, err := adapter.FetchIssuesByStates(ctx, profile.config.ActiveStates)
	if err != nil {
		t.Fatal("GitHub live sentinel fetch failed")
	}
	if len(issues) == 0 {
		t.Fatal("GitHub live repository has no active sentinel")
	}
	validateGitHubLiveIssues(t, issues, profile.config)

	sentinel := issues[0]
	reread, err := adapter.FetchIssuesByIDs(ctx, []string{sentinel.ID})
	if err != nil {
		t.Fatal("GitHub live sentinel re-read failed")
	}
	if len(reread) != 1 || !sameGitHubLiveIdentity(sentinel, reread[0], profile.config) {
		t.Fatal("GitHub live sentinel re-read did not preserve identity and scope")
	}
}

func validateGitHubLiveIssues(t *testing.T, issues []domain.Issue, config tracker.GitHubConfig) {
	t.Helper()
	wantedStates := map[string]struct{}{}
	for _, state := range config.ActiveStates {
		wantedStates[tracker.NormalizeState(state)] = struct{}{}
	}
	seenIDs := make(map[string]struct{}, len(issues))
	seenIdentifiers := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if err := issue.ValidateRequired(); err != nil {
			t.Fatal("GitHub live issue failed required-field validation")
		}
		if _, found := wantedStates[tracker.NormalizeState(issue.State)]; !found {
			t.Fatal("GitHub live issue was outside the requested states")
		}
		if _, duplicate := seenIDs[issue.ID]; duplicate {
			t.Fatal("GitHub live result contained a duplicate issue ID")
		}
		if _, duplicate := seenIdentifiers[issue.Identifier]; duplicate {
			t.Fatal("GitHub live result contained a duplicate issue identifier")
		}
		seenIDs[issue.ID] = struct{}{}
		seenIdentifiers[issue.Identifier] = struct{}{}
		if !validGitHubLiveNativeRef(issue, config) {
			t.Fatal("GitHub live issue native reference was outside repository scope")
		}
		if !validGitHubLiveCollections(issue) {
			t.Fatal("GitHub live issue collections were not normalized")
		}
		if _, err := json.Marshal(issue.NativeRef); err != nil {
			t.Fatal("GitHub live issue metadata was not JSON-safe")
		}
	}
}

func validGitHubLiveNativeRef(issue domain.Issue, config tracker.GitHubConfig) bool {
	if issue.NativeRef == nil || issue.NativeRef["owner"] != config.Owner || issue.NativeRef["repository"] != config.Repository {
		return false
	}
	number, ok := issue.NativeRef["number"].(json.Number)
	if !ok || !validGitHubLiveIssueNumber(number) || issue.ID != dispatchID(config, number.String()) || issue.Identifier != "#"+number.String() {
		return false
	}
	if databaseID, found := issue.NativeRef["database_id"]; found && !validGitHubLiveDatabaseID(databaseID) {
		return false
	}
	for _, key := range []string{"node_id", "state_reason"} {
		if value, found := issue.NativeRef[key]; found {
			text, ok := value.(string)
			if !ok || text == "" || strings.TrimSpace(text) != text {
				return false
			}
		}
	}
	allowed := map[string]struct{}{"owner": {}, "repository": {}, "number": {}, "database_id": {}, "node_id": {}, "state_reason": {}}
	for key := range issue.NativeRef {
		if _, found := allowed[key]; !found {
			return false
		}
	}
	return true
}

func validGitHubLiveIssueNumber(number json.Number) bool {
	text := number.String()
	if text == "" || strings.HasPrefix(text, "+") || (len(text) > 1 && text[0] == '0') {
		return false
	}
	value, err := strconv.ParseUint(text, 10, 64)
	return err == nil && value > 0
}

func validGitHubLiveDatabaseID(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	text := number.String()
	if text == "" || strings.HasPrefix(text, "+") || (len(text) > 1 && text[0] == '0') {
		return false
	}
	integer := new(big.Int)
	_, ok = integer.SetString(text, 10)
	return ok && integer.Sign() > 0
}

func validGitHubLiveCollections(issue domain.Issue) bool {
	return issue.Labels != nil && normalizedLiveLabels(issue.Labels) &&
		issue.BlockedBy != nil && len(issue.BlockedBy) == 0
}

func sameGitHubLiveIdentity(left, right domain.Issue, config tracker.GitHubConfig) bool {
	return left.ID == right.ID && left.Identifier == right.Identifier &&
		validGitHubLiveNativeRef(left, config) && validGitHubLiveNativeRef(right, config) &&
		left.NativeRef["number"] == right.NativeRef["number"]
}

func normalizedLiveLabels(labels []string) bool {
	return reflect.DeepEqual(labels, tracker.NormalizeLabels(labels))
}

func zeroGitHubBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

type githubLiveRedactingHandler struct {
	redactor *observability.Redactor
	delegate slog.Handler
}

func (handler githubLiveRedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.delegate.Enabled(ctx, level)
}

func (handler githubLiveRedactingHandler) Handle(ctx context.Context, record slog.Record) error {
	message, _ := handler.redactor.Value(record.Message).(string)
	sanitized := slog.NewRecord(record.Time, record.Level, message, record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		sanitized.AddAttrs(slog.Any(attribute.Key, handler.redactor.Value(attribute.Value.Any())))
		return true
	})
	return handler.delegate.Handle(ctx, sanitized)
}

func (handler githubLiveRedactingHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	sanitized := make([]slog.Attr, len(attributes))
	for index, attribute := range attributes {
		sanitized[index] = slog.Any(attribute.Key, handler.redactor.Value(attribute.Value.Any()))
	}
	handler.delegate = handler.delegate.WithAttrs(sanitized)
	return handler
}

func (handler githubLiveRedactingHandler) WithGroup(name string) slog.Handler {
	handler.delegate = handler.delegate.WithGroup(name)
	return handler
}
