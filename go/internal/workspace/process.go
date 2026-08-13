package workspace

import (
	"strings"
	"sync"
)

const maxHookOutputBytes = 1 << 20

type hookProcessSpec struct {
	Script      string
	WorkingDir  string
	Environment []string
}

type hookProcessResult struct {
	ExitCode int
	TimedOut bool
	Err      error
}

func deduplicateWindowsEnvironment(environment []string) []string {
	values := make([]string, 0, len(environment))
	seen := make(map[string]struct{}, len(environment))
	for index := len(environment) - 1; index >= 0; index-- {
		value := environment[index]
		separator := strings.IndexByte(value, '=')
		if separator == 0 {
			separator = strings.IndexByte(value[1:], '=') + 1
		}
		if separator < 0 {
			if value != "" {
				values = append(values, value)
			}
			continue
		}
		key := strings.ToLower(value[:separator])
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, value)
	}
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
	return values
}

type boundedOutput struct {
	mu        sync.Mutex
	contents  []byte
	limit     int
	truncated bool
}

func newBoundedOutput(limit int) *boundedOutput {
	if limit < 0 {
		limit = 0
	}
	return &boundedOutput{contents: make([]byte, 0, limit), limit: limit}
}

func (output *boundedOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	remaining := output.limit - len(output.contents)
	if remaining > 0 {
		keep := min(remaining, len(value))
		output.contents = append(output.contents, value[:keep]...)
	}
	if len(value) > remaining {
		output.truncated = true
	}
	return len(value), nil
}

func (output *boundedOutput) snapshot() ([]byte, bool) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return append([]byte(nil), output.contents...), output.truncated
}
