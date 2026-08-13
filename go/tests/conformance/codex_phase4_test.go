package conformance

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPhase4ProductionCompositionCannotSelectFakeAppServer(t *testing.T) {
	root := repositoryRoot(t)
	runSource := readFile(t, filepath.Join(root, "internal", "cli", "run.go"))
	productionSource := readFile(t, filepath.Join(root, "internal", "app", "production_agent.go"))
	if !strings.Contains(runSource, "app.ProductionAgentBuilder") {
		t.Fatal("enabled production runtime does not select ProductionAgentBuilder")
	}
	if !strings.Contains(productionSource, "Launch: codex.Launch") {
		t.Fatal("production builder does not select the contained Codex launcher")
	}
	for path, source := range map[string]string{
		"internal/cli/run.go":              runSource,
		"internal/app/production_agent.go": productionSource,
	} {
		if strings.Contains(source, "fakeappserver") || strings.Contains(source, "SYMPHONY_FAKE_CODEX") {
			t.Fatalf("%s contains a production fake-app-server selection path", path)
		}
	}
}

func TestPhase4RealSmokeIsExplicitlyOptInAndUsesNoSyntheticPassFlag(t *testing.T) {
	root := repositoryRoot(t)
	source := readFile(t, filepath.Join(root, "internal", "codex", "real_smoke_test.go"))
	for _, required := range []string{
		`os.Getenv("SYMPHONY_REAL_CODEX_SMOKE") != "1"`,
		`t.Skip("SKIPPED: real Codex smoke")`,
		`SYMPHONY_REAL_CODEX_WORKFLOW`,
		`codex login status`,
		`runner.Preflight`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("real smoke is missing %q", required)
		}
	}
	for _, forbidden := range []string{"SYNTHETIC_PASS", "FAKE_SUCCESS", "assumeCompatible"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("real smoke contains forbidden synthetic success path %q", forbidden)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
