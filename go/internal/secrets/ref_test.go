package secrets

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRefUsesStableNonSecretNames(t *testing.T) {
	ref := Ref{WorkflowID: "c3a1", TrackerKind: "github"}
	if ref.Service() != "symphony/workflow/c3a1" || ref.Account() != "tracker/github" {
		t.Fatalf("unexpected keyring names: %q %q", ref.Service(), ref.Account())
	}
}

func TestStatusNeverContainsCredential(t *testing.T) {
	fake := NewMemoryStore()
	ref := Ref{WorkflowID: "w", TrackerKind: "linear"}
	if err := fake.Put(context.Background(), ref, []byte("secret-canary")); err != nil {
		t.Fatal(err)
	}
	got := fake.Status(context.Background(), ref)
	if !got.Present || strings.Contains(fmt.Sprintf("%+v", got), "secret-canary") {
		t.Fatalf("unsafe status: %#v", got)
	}
}
