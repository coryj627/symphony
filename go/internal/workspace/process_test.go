package workspace

import (
	"slices"
	"testing"
)

func TestDeduplicateWindowsEnvironmentKeepsLastCaseInsensitiveKey(t *testing.T) {
	environment := []string{
		"Path=C:\\first",
		"ORDINARY=first",
		"PATH=C:\\second",
		"ordinary=second",
		`=C:=C:\first`,
		`=D:=D:\only`,
		`=c:=C:\second`,
		"malformed-entry",
	}
	want := []string{
		"PATH=C:\\second",
		"ordinary=second",
		`=D:=D:\only`,
		`=c:=C:\second`,
		"malformed-entry",
	}
	if got := deduplicateWindowsEnvironment(environment); !slices.Equal(got, want) {
		t.Fatalf("deduplicated environment = %#v, want %#v", got, want)
	}
	if !slices.Equal(environment, []string{
		"Path=C:\\first",
		"ORDINARY=first",
		"PATH=C:\\second",
		"ordinary=second",
		`=C:=C:\first`,
		`=D:=D:\only`,
		`=c:=C:\second`,
		"malformed-entry",
	}) {
		t.Fatalf("input environment mutated: %#v", environment)
	}
}
