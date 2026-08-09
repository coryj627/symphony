//go:build integration_live

package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

const (
	linearLiveEnableEnv  = "SYMPHONY_RUN_LINEAR_LIVE"
	linearLiveProjectEnv = "SYMPHONY_LINEAR_TEST_PROJECT"
	linearLiveTokenEnv   = "SYMPHONY_LINEAR_TEST_TOKEN"
	linearLiveEndpoint   = "https://api.linear.app/graphql"
)

type linearLiveProfile struct {
	enabled bool
	config  tracker.LinearConfig
	token   []byte
}

type linearLiveLookup func(string) (string, bool)

func loadLinearLiveProfile(lookup linearLiveLookup) (linearLiveProfile, error) {
	if lookup == nil {
		return linearLiveProfile{}, errors.New("Linear live environment lookup is unavailable")
	}
	enable, found := lookup(linearLiveEnableEnv)
	if !found || enable == "0" {
		return linearLiveProfile{}, nil
	}
	if enable != "1" {
		return linearLiveProfile{}, errors.New("Linear live enable flag must be 0 or 1")
	}
	project, found := lookup(linearLiveProjectEnv)
	if !found || !validLinearLiveProjectSlug(project) {
		return linearLiveProfile{}, errors.New("Linear live project slug is required and must be valid")
	}
	raw := workflow.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"project_slug": project, "credential_ref": "$" + linearLiveTokenEnv,
		},
	}
	decoded, err := tracker.DecodeConfig(raw)
	if err != nil {
		return linearLiveProfile{}, errors.New("Linear live project configuration is invalid")
	}
	config, ok := decoded.(tracker.LinearConfig)
	if !ok || config.Endpoint != linearLiveEndpoint {
		return linearLiveProfile{}, errors.New("Linear live tracker configuration is invalid")
	}
	tokenText, found := lookup(linearLiveTokenEnv)
	if !found || strings.TrimSpace(tokenText) == "" {
		return linearLiveProfile{}, errors.New("Linear live token is required")
	}
	// Environment lookup exposes an immutable string that Go cannot reliably
	// erase. Copy it exactly once into memory owned and cleared by this test.
	ownedToken := []byte(tokenText)
	return linearLiveProfile{enabled: true, config: config, token: ownedToken}, nil
}

func validLinearLiveProjectSlug(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/?#") {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return true
}

func TestLinearLiveProfileConfiguration(t *testing.T) {
	const canary = "linear-live-token-canary"
	for _, test := range []struct {
		name        string
		environment map[string]string
		enabled     bool
		wantError   bool
		wantLookups []string
	}{
		{name: "missing enable", environment: map[string]string{}, wantLookups: []string{linearLiveEnableEnv}},
		{name: "zero disabled", environment: map[string]string{linearLiveEnableEnv: "0", linearLiveTokenEnv: canary}, wantLookups: []string{linearLiveEnableEnv}},
		{name: "empty enable", environment: map[string]string{linearLiveEnableEnv: "", linearLiveTokenEnv: canary}, wantError: true, wantLookups: []string{linearLiveEnableEnv}},
		{name: "whitespace enable", environment: map[string]string{linearLiveEnableEnv: " 1 ", linearLiveTokenEnv: canary}, wantError: true, wantLookups: []string{linearLiveEnableEnv}},
		{name: "case enable", environment: map[string]string{linearLiveEnableEnv: "TRUE", linearLiveTokenEnv: canary}, wantError: true, wantLookups: []string{linearLiveEnableEnv}},
		{name: "numeric enable", environment: map[string]string{linearLiveEnableEnv: "01", linearLiveTokenEnv: canary}, wantError: true, wantLookups: []string{linearLiveEnableEnv}},
		{name: "missing project", environment: map[string]string{linearLiveEnableEnv: "1", linearLiveTokenEnv: canary}, wantError: true, wantLookups: []string{linearLiveEnableEnv, linearLiveProjectEnv}},
		{name: "blank project", environment: map[string]string{linearLiveEnableEnv: "1", linearLiveProjectEnv: " ", linearLiveTokenEnv: canary}, wantError: true, wantLookups: []string{linearLiveEnableEnv, linearLiveProjectEnv}},
		{name: "missing token", environment: map[string]string{linearLiveEnableEnv: "1", linearLiveProjectEnv: "symphony"}, wantError: true, wantLookups: []string{linearLiveEnableEnv, linearLiveProjectEnv, linearLiveTokenEnv}},
		{name: "blank token", environment: map[string]string{linearLiveEnableEnv: "1", linearLiveProjectEnv: "symphony", linearLiveTokenEnv: " "}, wantError: true, wantLookups: []string{linearLiveEnableEnv, linearLiveProjectEnv, linearLiveTokenEnv}},
		{name: "valid", environment: map[string]string{linearLiveEnableEnv: "1", linearLiveProjectEnv: "symphony", linearLiveTokenEnv: canary}, enabled: true, wantLookups: []string{linearLiveEnableEnv, linearLiveProjectEnv, linearLiveTokenEnv}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups := []string{}
			profile, err := loadLinearLiveProfile(func(name string) (string, bool) {
				lookups = append(lookups, name)
				value, found := test.environment[name]
				return value, found
			})
			defer zeroLinearBytes(profile.token)
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
				if string(profile.token) != canary || profile.config.ProjectSlug != "symphony" || profile.config.Endpoint != linearLiveEndpoint || profile.config.CredentialRef != "$"+linearLiveTokenEnv {
					t.Fatal("valid configuration did not preserve the selected profile")
				}
			}
		})
	}
}

func TestLinearLiveProfileRejectsMalformedProjectWithoutTokenLookup(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	for index, project := range []string{
		" ", " symphony", "symphony ", "https://linear.app/project/symphony", "symphony/project",
		"symphony?query", "symphony\nproject", invalidUTF8, strings.Repeat("x", 257),
	} {
		t.Run(jsonNumber(index), func(t *testing.T) {
			lookedUpToken := false
			_, err := loadLinearLiveProfile(func(name string) (string, bool) {
				switch name {
				case linearLiveEnableEnv:
					return "1", true
				case linearLiveProjectEnv:
					return project, true
				case linearLiveTokenEnv:
					lookedUpToken = true
					return "token-canary", true
				default:
					return "", false
				}
			})
			if err == nil || lookedUpToken || strings.Contains(err.Error(), "token-canary") {
				t.Fatalf("malformed project was not rejected safely: error=%v token_lookup=%t", err, lookedUpToken)
			}
		})
	}
}

func TestLinearLiveCollectionsMatchProductionBlockerNormalization(t *testing.T) {
	for _, test := range []struct {
		name      string
		blockedBy []domain.BlockerRef
		labels    []string
		wantValid bool
	}{
		{name: "normalized empty", blockedBy: []domain.BlockerRef{}, labels: []string{"sentinel"}, wantValid: true},
		{name: "normalized nonempty", blockedBy: []domain.BlockerRef{{ID: stringPointer("blocker-1")}}, labels: []string{"sentinel"}, wantValid: true},
		{name: "nil blockers", blockedBy: nil, labels: []string{"sentinel"}},
		{name: "empty blocker", blockedBy: []domain.BlockerRef{{}}, labels: []string{"sentinel"}},
		{name: "blank-only blocker", blockedBy: []domain.BlockerRef{{State: stringPointer(" ")}}, labels: []string{"sentinel"}},
		{name: "unnormalized labels", blockedBy: []domain.BlockerRef{}, labels: []string{" Sentinel "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := domain.Issue{
				ID: "issue-1", Identifier: "LIN-1", Title: "Sentinel", State: "Todo",
				Labels: test.labels, BlockedBy: test.blockedBy,
			}
			if got := validLinearLiveCollections(issue); got != test.wantValid {
				t.Fatalf("collection validity = %t, want %t", got, test.wantValid)
			}
		})
	}
}

func TestLinearLiveProfile(t *testing.T) {
	profile, err := loadLinearLiveProfile(os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.enabled {
		t.Skip("SKIPPED: Linear live profile not enabled")
	}
	defer zeroLinearBytes(profile.token)
	if err := os.Unsetenv(linearLiveTokenEnv); err != nil {
		t.Fatal("Linear live token could not be removed from the environment")
	}
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret(profile.token)
	logger := slog.New(linearLiveRedactingHandler{redactor: redactor, delegate: slog.NewJSONHandler(io.Discard, nil)})

	adapter, err := New(profile.config, profile.token, nil, logger)
	if err != nil {
		t.Fatal("Linear live adapter could not be created")
	}
	defer zeroLinearBytes(adapter.token)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	issues, err := adapter.FetchIssuesByStates(ctx, profile.config.ActiveStates)
	if err != nil {
		t.Fatal("Linear live sentinel fetch failed")
	}
	if len(issues) == 0 {
		t.Fatal("Linear live project has no active sentinel")
	}
	validateLinearLiveIssues(t, issues, profile.config)

	sentinel := issues[0]
	reread, err := adapter.FetchIssuesByIDs(ctx, []string{sentinel.ID})
	if err != nil {
		t.Fatal("Linear live sentinel re-read failed")
	}
	if len(reread) != 1 || !sameLinearLiveIdentity(sentinel, reread[0], profile.config) {
		t.Fatal("Linear live sentinel re-read did not preserve identity and scope")
	}
}

func validateLinearLiveIssues(t *testing.T, issues []domain.Issue, config tracker.LinearConfig) {
	t.Helper()
	wantedStates := map[string]struct{}{}
	for _, state := range config.ActiveStates {
		wantedStates[tracker.NormalizeState(state)] = struct{}{}
	}
	seenIDs := make(map[string]struct{}, len(issues))
	seenIdentifiers := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if err := issue.ValidateRequired(); err != nil {
			t.Fatal("Linear live issue failed required-field validation")
		}
		if _, found := wantedStates[tracker.NormalizeState(issue.State)]; !found {
			t.Fatal("Linear live issue was outside the requested states")
		}
		if _, duplicate := seenIDs[issue.ID]; duplicate {
			t.Fatal("Linear live result contained a duplicate issue ID")
		}
		if _, duplicate := seenIdentifiers[issue.Identifier]; duplicate {
			t.Fatal("Linear live result contained a duplicate issue identifier")
		}
		seenIDs[issue.ID] = struct{}{}
		seenIdentifiers[issue.Identifier] = struct{}{}
		if !validLinearLiveNativeRef(issue, config) {
			t.Fatal("Linear live issue native reference was outside project scope")
		}
		if !validLinearLiveCollections(issue) {
			t.Fatal("Linear live issue collections were not normalized")
		}
		if _, err := json.Marshal(issue.NativeRef); err != nil {
			t.Fatal("Linear live issue metadata was not JSON-safe")
		}
	}
}

func validLinearLiveNativeRef(issue domain.Issue, config tracker.LinearConfig) bool {
	if issue.NativeRef == nil || issue.NativeRef["issue_id"] != issue.ID || issue.NativeRef["identifier"] != issue.Identifier || issue.NativeRef["project_slug"] != config.ProjectSlug {
		return false
	}
	if strings.TrimSpace(stringValue(issue.NativeRef["project_id"])) == "" || strings.TrimSpace(stringValue(issue.NativeRef["team_id"])) == "" {
		return false
	}
	wantKeys := map[string]struct{}{"issue_id": {}, "identifier": {}, "project_id": {}, "project_slug": {}, "team_id": {}}
	if len(issue.NativeRef) != len(wantKeys) {
		return false
	}
	for key := range issue.NativeRef {
		if _, found := wantKeys[key]; !found {
			return false
		}
	}
	return true
}

func validLinearLiveCollections(issue domain.Issue) bool {
	if issue.Labels == nil || !reflect.DeepEqual(issue.Labels, tracker.NormalizeLabels(issue.Labels)) || issue.BlockedBy == nil {
		return false
	}
	normalized, err := tracker.NormalizeIssue(issue)
	return err == nil && reflect.DeepEqual(issue.BlockedBy, normalized.BlockedBy)
}

func sameLinearLiveIdentity(left, right domain.Issue, config tracker.LinearConfig) bool {
	return left.ID == right.ID && left.Identifier == right.Identifier &&
		validLinearLiveNativeRef(left, config) && validLinearLiveNativeRef(right, config) &&
		left.NativeRef["project_id"] == right.NativeRef["project_id"]
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func zeroLinearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

type linearLiveRedactingHandler struct {
	redactor *observability.Redactor
	delegate slog.Handler
}

func (handler linearLiveRedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.delegate.Enabled(ctx, level)
}

func (handler linearLiveRedactingHandler) Handle(ctx context.Context, record slog.Record) error {
	message, _ := handler.redactor.Value(record.Message).(string)
	sanitized := slog.NewRecord(record.Time, record.Level, message, record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		sanitized.AddAttrs(slog.Any(attribute.Key, handler.redactor.Value(attribute.Value.Any())))
		return true
	})
	return handler.delegate.Handle(ctx, sanitized)
}

func (handler linearLiveRedactingHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	sanitized := make([]slog.Attr, len(attributes))
	for index, attribute := range attributes {
		sanitized[index] = slog.Any(attribute.Key, handler.redactor.Value(attribute.Value.Any()))
	}
	handler.delegate = handler.delegate.WithAttrs(sanitized)
	return handler
}

func (handler linearLiveRedactingHandler) WithGroup(name string) slog.Handler {
	handler.delegate = handler.delegate.WithGroup(name)
	return handler
}
