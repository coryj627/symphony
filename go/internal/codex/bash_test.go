package codex

import (
	"errors"
	"reflect"
	"testing"
)

func TestBashReceivesCommandAsOneArgument(t *testing.T) {
	spec := BashCommand("/safe/bash", "codex app-server --config 'x y'")
	if spec.Path != "/safe/bash" {
		t.Fatalf("Path = %q", spec.Path)
	}
	want := []string{"-lc", "codex app-server --config 'x y'"}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Fatalf("Args = %q, want %q", spec.Args, want)
	}
}

func TestBashCommandNeverConcatenatesExternalValues(t *testing.T) {
	command := "codex app-server"
	spec := BashCommand("/Applications/Git/bin/bash", command)
	for _, unwanted := range []string{"/workspace/from-browser", "http://localhost:3000"} {
		for _, argument := range spec.Args {
			if argument == unwanted {
				t.Fatalf("external value %q became an argument: %q", unwanted, spec.Args)
			}
		}
	}
	if spec.Args[1] != command {
		t.Fatalf("workflow command changed: %q", spec.Args[1])
	}
}

func TestFindBashReportsActionableUnsupportedPlatform(t *testing.T) {
	_, err := findBash("linux", func(string) (string, bool) { return "", false }, func(string) bool { return false })
	if !errors.Is(err, ErrBashUnavailable) || err == nil || err.Error() == ErrBashUnavailable.Error() {
		t.Fatalf("findBash() error = %v", err)
	}
}

func TestFindBashUsesMacOSSystemBash(t *testing.T) {
	got, err := findBash("darwin", nil, func(path string) bool { return path == "/bin/bash" })
	if err != nil || got != "/bin/bash" {
		t.Fatalf("findBash() = %q, %v", got, err)
	}
}

func TestFindBashDiscoversGitForWindows(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "ProgramFiles" {
			return `C:\Program Files`, true
		}
		return "", false
	}
	got, err := findBash("windows", lookup, func(path string) bool {
		return path == `C:\Program Files\Git\bin\bash.exe`
	})
	if err != nil || got != `C:\Program Files\Git\bin\bash.exe` {
		t.Fatalf("findBash() = %q, %v", got, err)
	}
}
