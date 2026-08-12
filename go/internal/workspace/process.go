package workspace

import (
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
