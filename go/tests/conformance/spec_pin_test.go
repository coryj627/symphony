package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedSpecificationsHaveReviewedDigests(t *testing.T) {
	root := repositoryRoot(t)
	assertFileSHA256(t, filepath.Join(root, "..", "SPEC.md"), "adb93eb2349ccbf39a8ca389ac29c0f2034f1204776319b4535a2f9424f4322d")
	assertFileSHA256(t, filepath.Join(root, "..", "docs", "superpowers", "specs", "2026-08-06-symphony-accessible-cross-platform-design.md"), "801a8ab935e78faaa6a89794d623674684bff3a7f237699a251be5c802c09c00")
}

func assertFileSHA256(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if got := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("%s SHA-256 = %s, want reviewed %s", path, got, want)
	}
}
