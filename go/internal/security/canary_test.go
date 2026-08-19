package security

import (
	"strings"
	"testing"
)

func TestCanaryIsUniqueAndScannerReady(t *testing.T) {
	first := NewCanary(t)
	second := NewCanary(t)

	if first.Value == second.Value {
		t.Fatal("independent disposable canaries were equal")
	}
	if !strings.HasPrefix(first.Value, canaryPrefix) {
		t.Fatal("disposable canary omitted its recognizable test-only prefix")
	}
	if len(first.Value) < 32 {
		t.Fatal("disposable canary did not contain enough entropy")
	}

	scanner, err := NewArtifactScanner([]byte(first.Value))
	if err != nil {
		t.Fatalf("NewArtifactScanner() failed: %v", err)
	}
	findings, err := scanner.Scan(TextArtifact("safe control", "ordinary observable issue content"))
	if err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("safe control produced %d canary findings", len(findings))
	}
}

func TestCanaryGenerationFailsClosedWithoutEnoughEntropy(t *testing.T) {
	if _, err := newCanary(strings.NewReader("short")); err == nil {
		t.Fatal("newCanary() accepted incomplete entropy")
	}
}
