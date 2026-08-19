package security

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanaryArtifactScannerFindsRawAndEncodedFormsWithoutDisclosingValue(t *testing.T) {
	canary := NewCanary(t)
	secret := canary.Value + " +/?\""
	scanner, err := NewArtifactScanner([]byte(secret))
	if err != nil {
		t.Fatalf("NewArtifactScanner() failed: %v", err)
	}
	jsonValue, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}
	encodedForms := []string{
		secret,
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawStdEncoding.EncodeToString([]byte(secret)),
		base64.URLEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)),
		hex.EncodeToString([]byte(secret)),
		strings.ToUpper(hex.EncodeToString([]byte(secret))),
		url.QueryEscape(secret),
		string(jsonValue[1 : len(jsonValue)-1]),
	}
	for index, encoded := range encodedForms {
		findings, scanErr := scanner.Scan(TextArtifact(fmt.Sprintf("encoded response %d", index), encoded))
		if scanErr != nil {
			t.Fatalf("Scan() failed for encoded form %d: %v", index, scanErr)
		}
		if len(findings) == 0 {
			t.Fatalf("Scan() missed encoded form %d", index)
		}
		if rendered := fmt.Sprint(findings); strings.Contains(rendered, secret) {
			t.Fatal("scanner findings disclosed the disposable canary")
		}
	}

	findings, err := scanner.Scan(TextArtifact("artifact-"+secret, "safe content"))
	if err != nil {
		t.Fatalf("name Scan() failed: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("Scan() missed a canary-bearing artifact name")
	}
	if rendered := fmt.Sprint(findings); strings.Contains(rendered, secret) {
		t.Fatal("scanner name finding disclosed the disposable canary")
	}
}

func TestCanaryArtifactScannerScansEnvironmentAndDirectoryTrees(t *testing.T) {
	canary := NewCanary(t)
	scanner, err := NewArtifactScanner([]byte(canary.Value))
	if err != nil {
		t.Fatalf("NewArtifactScanner() failed: %v", err)
	}

	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	leakPath := filepath.Join(nested, "capture.txt")
	if err := os.WriteFile(leakPath, []byte(canary.Value), 0o600); err != nil {
		t.Fatal(err)
	}

	findings, err := scanner.Scan(
		StringsArtifact("child environment", []string{"SAFE=yes", "TOKEN=" + canary.Value}),
		PathArtifact("captured test artifacts", root),
	)
	if err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}
	if len(findings) < 2 {
		t.Fatalf("Scan() findings = %d, want at least 2", len(findings))
	}

	if err := os.WriteFile(leakPath, []byte("ordinary observable issue content"), 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err = scanner.Scan(
		StringsArtifact("child environment", []string{"SAFE=yes"}),
		PathArtifact("captured test artifacts", root),
	)
	if err != nil {
		t.Fatalf("clean Scan() failed: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("clean artifacts produced %d canary findings", len(findings))
	}
}

func TestCanaryArtifactScannerFailsClosedForUninspectedFiles(t *testing.T) {
	canary := NewCanary(t)
	scanner, err := NewArtifactScanner([]byte(canary.Value))
	if err != nil {
		t.Fatalf("NewArtifactScanner() failed: %v", err)
	}
	scanner.maxFileBytes = 4

	path := filepath.Join(t.TempDir(), "oversize.bin")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(PathArtifact("bounded artifact", path)); err == nil {
		t.Fatal("Scan() accepted a file it could not inspect completely")
	}
	if _, err := scanner.Scan(BytesArtifact("bounded memory", []byte("12345"))); err == nil {
		t.Fatal("Scan() accepted an in-memory artifact it could not inspect within limits")
	}

	scanner.maxFileBytes = 8
	scanner.maxTotalBytes = 8
	if _, err := scanner.Scan(
		BytesArtifact("first memory artifact", []byte("12345")),
		BytesArtifact("second memory artifact", []byte("67890")),
	); err == nil {
		t.Fatal("Scan() accepted an artifact set beyond the total byte limit")
	}
}

func TestCanaryArtifactScannerCopiesInputsAndRequiresArtifacts(t *testing.T) {
	canary := NewCanary(t)
	secret := []byte(canary.Value)
	scanner, err := NewArtifactScanner(secret)
	if err != nil {
		t.Fatalf("NewArtifactScanner() failed: %v", err)
	}
	artifactBytes := append([]byte("prefix "), secret...)
	artifact := BytesArtifact("copied artifact", artifactBytes)
	for index := range secret {
		secret[index] = 0
	}
	for index := range artifactBytes {
		artifactBytes[index] = 0
	}

	findings, err := scanner.Scan(artifact)
	if err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("scanner or artifact did not retain its defensive input copy")
	}
	if _, err := scanner.Scan(); err == nil {
		t.Fatal("Scan() accepted an empty artifact set")
	}
	if _, err := scanner.Scan(Artifact{}); err == nil {
		t.Fatal("Scan() accepted an artifact not created by a constructor")
	}
}
