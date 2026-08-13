package codex

import (
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/observability"
)

const (
	maxStderrBytes        = 256 << 10
	maxStderrLogLines     = 32
	maxStderrLogLineBytes = 4 << 10
)

// StderrCapture retains only a bounded, sanitized diagnostic tail.
type StderrCapture struct {
	mu       sync.Mutex
	redactor *observability.Redactor
	logger   *slog.Logger
	tail     []byte
	pending  []byte
}

func NewStderrCapture(redactor *observability.Redactor, logger *slog.Logger) *StderrCapture {
	if redactor == nil {
		redactor = observability.NewRedactor(nil, nil)
	}
	return &StderrCapture{redactor: redactor, logger: logger}
}

func (capture *StderrCapture) Write(source []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.pending = append(capture.pending, source...)
	for {
		newline := strings.IndexByte(string(capture.pending), '\n')
		if newline < 0 {
			break
		}
		line := append([]byte(nil), capture.pending[:newline+1]...)
		capture.pending = append(capture.pending[:0], capture.pending[newline+1:]...)
		capture.appendSanitized(line)
	}
	if len(capture.pending) > maxStderrBytes {
		capture.pending = append([]byte(nil), capture.pending[len(capture.pending)-maxStderrBytes:]...)
	}
	return len(source), nil
}

func (capture *StderrCapture) Diagnostic() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	combined := append([]byte(nil), capture.tail...)
	combined = append(combined, []byte(capture.sanitize(capture.pending))...)
	combined = boundedUTF8Tail(combined, maxStderrBytes)
	return string(combined)
}

func (capture *StderrCapture) LogSummary() {
	if capture == nil || capture.logger == nil {
		return
	}
	lines := strings.Split(capture.Diagnostic(), "\n")
	nonempty := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonempty = append(nonempty, line)
		}
	}
	if len(nonempty) == 0 {
		return
	}
	if len(nonempty) > maxStderrLogLines {
		nonempty = nonempty[len(nonempty)-maxStderrLogLines:]
	}
	for _, line := range nonempty {
		line = string(boundedUTF8Tail([]byte(line), maxStderrLogLineBytes))
		capture.logger.Warn("codex_stderr", "line", line)
	}
}

func (capture *StderrCapture) appendSanitized(raw []byte) {
	capture.tail = append(capture.tail, []byte(capture.sanitize(raw))...)
	capture.tail = boundedUTF8Tail(capture.tail, maxStderrBytes)
}

func (capture *StderrCapture) sanitize(raw []byte) string {
	value := strings.ToValidUTF8(string(raw), "")
	if sanitized, ok := capture.redactor.Value(value).(string); ok {
		return sanitized
	}
	return "[UNSAFE VALUE]"
}

func boundedUTF8Tail(value []byte, maximum int) []byte {
	if len(value) <= maximum {
		return value
	}
	value = append([]byte(nil), value[len(value)-maximum:]...)
	for len(value) > 0 && !utf8.RuneStart(value[0]) {
		value = value[1:]
	}
	return value
}
