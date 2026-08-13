package codex

import (
	"reflect"
	"testing"
)

func TestChildEnvironmentRemovesEveryTrackerSecretName(t *testing.T) {
	environment := []string{
		"PATH=/bin",
		"GH_TOKEN=one",
		"GITHUB_TOKEN=two",
		"LINEAR_API_KEY=three",
		"CUSTOM_TRACKER=four",
		"SAFE=yes",
	}
	got := SanitizeEnvironment(environment, []string{"CUSTOM_TRACKER"})
	want := []string{"PATH=/bin", "SAFE=yes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SanitizeEnvironment() = %q, want %q", got, want)
	}
}

func TestChildEnvironmentNormalizesCredentialReferencesAndDuplicates(t *testing.T) {
	environment := []string{
		"SAFE=old",
		"$INVALID=preserved",
		"CUSTOM_SECRET=one",
		"SAFE=new",
		"custom_secret=two",
		"NO_EQUALS",
	}
	got := sanitizeEnvironmentForOS(environment, []string{"$CUSTOM_SECRET", "", "not-valid!"}, "darwin")
	want := []string{"$INVALID=preserved", "SAFE=new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeEnvironmentForOS() = %q, want %q", got, want)
	}
}

func TestChildEnvironmentUsesCaseInsensitiveKeysOnWindows(t *testing.T) {
	environment := []string{
		"Path=first",
		"PATH=second",
		"github_token=secret",
		"Safe=yes",
	}
	got := sanitizeEnvironmentForOS(environment, nil, "windows")
	want := []string{"PATH=second", "Safe=yes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeEnvironmentForOS() = %q, want %q", got, want)
	}
}

func TestSecretEnvironmentNameValidation(t *testing.T) {
	for _, value := range []string{"TOKEN", "$TOKEN", "_TOKEN_2"} {
		if _, ok := normalizeSecretEnvironmentName(value); !ok {
			t.Errorf("normalizeSecretEnvironmentName(%q) rejected a valid name", value)
		}
	}
	for _, value := range []string{"", "$", "2TOKEN", "TOKEN-NAME", "TOKEN=VALUE", " TOKEN"} {
		if _, ok := normalizeSecretEnvironmentName(value); ok {
			t.Errorf("normalizeSecretEnvironmentName(%q) accepted an invalid name", value)
		}
	}
}
