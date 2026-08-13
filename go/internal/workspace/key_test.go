package workspace

import (
	"errors"
	"regexp"
	"testing"
)

func TestChangedIdentifiersUseDistinctStableHashSuffixes(t *testing.T) {
	a, err := Key("A/B")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Key("A?B")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`^A_B-[0-9a-f]{16}$`)
	if a == b || !pattern.MatchString(a) || !pattern.MatchString(b) {
		t.Fatalf("unsafe keys: %q %q", a, b)
	}
	again, err := Key("A/B")
	if err != nil || again != a {
		t.Fatalf("key is not stable: %q, %v", again, err)
	}
	unchanged, err := Key("ABC-123._ok")
	if err != nil || unchanged != "ABC-123._ok" {
		t.Fatalf("unchanged identifier = %q, %v", unchanged, err)
	}
}

func TestKeyRejectsValuesThatCouldNameTheRoot(t *testing.T) {
	for _, identifier := range []string{"", ".", ".."} {
		t.Run(identifier, func(t *testing.T) {
			if _, err := Key(identifier); !errors.Is(err, ErrInvalidWorkspaceKey) {
				t.Fatalf("Key(%q) error = %v", identifier, err)
			}
		})
	}
}

func TestKeySanitizesEveryUnsafeRuneWithoutPathComponents(t *testing.T) {
	key, err := Key(`/absolute\path/é/issue`)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^_absolute_path___issue-[0-9a-f]{16}$`).MatchString(key) {
		t.Fatalf("Key() = %q", key)
	}
}
