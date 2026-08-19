package security

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"testing"
)

const (
	canaryPrefix       = "symphony_test_canary_"
	canaryEntropyBytes = 24
)

// Canary is a disposable, test-only secret marker. Value must only be seeded
// through the secret path under test and must never be printed by a test.
type Canary struct {
	Value string
}

// NewCanary creates a unique disposable marker and clears the returned holder
// during test cleanup. Copies seeded into the system under test remain the
// caller's responsibility and must not be persisted outside isolated fixtures.
func NewCanary(t testing.TB) *Canary {
	t.Helper()
	canary, err := newCanary(rand.Reader)
	if err != nil {
		t.Fatalf("create disposable secret canary: %v", err)
	}
	t.Cleanup(func() {
		canary.Value = ""
	})
	return canary
}

func newCanary(source io.Reader) (*Canary, error) {
	if source == nil {
		return nil, errors.New("canary entropy source is required")
	}
	entropy := make([]byte, canaryEntropyBytes)
	if _, err := io.ReadFull(source, entropy); err != nil {
		return nil, errors.New("canary entropy unavailable")
	}
	return &Canary{Value: canaryPrefix + base64.RawURLEncoding.EncodeToString(entropy)}, nil
}

// AssertAbsent fails the test with artifact names and representation classes,
// never the canary or matching artifact bytes.
func (canary *Canary) AssertAbsent(t testing.TB, artifacts ...Artifact) {
	t.Helper()
	if canary == nil {
		t.Fatal("disposable secret canary is required")
	}
	scanner, err := NewArtifactScanner([]byte(canary.Value))
	if err != nil {
		t.Fatalf("create disposable canary scanner: %v", err)
	}
	findings, err := scanner.Scan(artifacts...)
	if err != nil {
		t.Fatalf("scan disposable canary artifacts: %v", err)
	}
	for _, finding := range findings {
		t.Errorf(
			"disposable canary detected in %q at %s (%s representation)",
			finding.Artifact,
			finding.Location,
			finding.Representation,
		)
	}
}
