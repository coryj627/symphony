package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedSpecificationsHaveReviewedDigests(t *testing.T) {
	root := repositoryRoot(t)
	assertFileSHA256(t, filepath.Join(root, "..", "SPEC.md"), "29d6b45a85453e045883c064c0e08595f9d4a33f9a2527f649bc1363b74e0176")
	assertFileSHA256(t, filepath.Join(root, "..", "docs", "superpowers", "specs", "2026-08-06-symphony-accessible-cross-platform-design.md"), "c566bfb531bdd94a2be961748f652bfd143e97af7856e6029022623843da7267")
}

func assertFileSHA256(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	canonical := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	digest := sha256.Sum256([]byte(canonical))
	if got := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("%s SHA-256 = %s, want reviewed %s", path, got, want)
	}
}
