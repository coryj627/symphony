package codex

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

var ErrBashUnavailable = errors.New("codex_bash_unavailable")

// CommandSpec is an executable path and its already-separated arguments.
type CommandSpec struct {
	Path string
	Args []string
}

// BashCommand preserves the workflow-owned command as exactly one argument.
func BashCommand(path, command string) CommandSpec {
	return CommandSpec{Path: path, Args: []string{"-lc", command}}
}

// FindBash locates the supported native Bash implementation.
func FindBash() (string, error) {
	return findBash(runtime.GOOS, os.LookupEnv, executableFile)
}

func findBash(goos string, lookup func(string) (string, bool), exists func(string) bool) (string, error) {
	switch goos {
	case "darwin":
		if exists != nil && exists("/bin/bash") {
			return "/bin/bash", nil
		}
		return "", fmt.Errorf("%w: macOS system Bash was not found at /bin/bash", ErrBashUnavailable)
	case "windows":
		candidates := make([]string, 0, 8)
		for _, environmentName := range []string{"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)", "LocalAppData"} {
			if lookup == nil {
				continue
			}
			base, ok := lookup(environmentName)
			if !ok || base == "" {
				continue
			}
			if environmentName == "LocalAppData" {
				candidates = append(candidates, base+`\Programs\Git\bin\bash.exe`, base+`\Programs\Git\usr\bin\bash.exe`)
			} else {
				candidates = append(candidates, base+`\Git\bin\bash.exe`, base+`\Git\usr\bin\bash.exe`)
			}
		}
		candidates = append(candidates,
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files\Git\usr\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		)
		for _, candidate := range candidates {
			if exists != nil && exists(candidate) {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("%w: install Git for Windows and enable its Bash executable", ErrBashUnavailable)
	default:
		return "", fmt.Errorf("%w: Symphony supports Codex launch only on macOS and Windows", ErrBashUnavailable)
	}
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}
