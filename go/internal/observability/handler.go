package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Options configures the process-scoped structured logger.
type Options struct {
	DataDir  string
	Redactor *Redactor
	Stderr   io.Writer
}

// NewLogger creates a sanitized JSON logger and its bounded query store.
func NewLogger(options Options) (*slog.Logger, *LogStore, error) {
	if strings.TrimSpace(options.DataDir) == "" || !filepath.IsAbs(options.DataDir) {
		return nil, nil, errors.New("observability data directory must be absolute")
	}
	redactor := options.Redactor
	if redactor == nil {
		redactor = NewRedactor(nil, nil)
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	var sink lineSink
	logDirectory := filepath.Join(filepath.Clean(options.DataDir), "logs")
	initializationErr := ensureSecureDirectory(logDirectory)
	if initializationErr == nil {
		sink, initializationErr = newRotatingWriter(
			filepath.Join(logDirectory, "symphony.jsonl"),
			defaultActiveLogSize,
			defaultLogArchives,
		)
	}
	store := newLogStore(sink, stderr)
	if initializationErr != nil {
		store.markDegraded()
	}
	delegate := slog.NewJSONHandler(io.Discard, nil)
	return slog.New(newSanitizingHandler(store, redactor, delegate)), store, nil
}

type handlerOperation struct {
	attrs []slog.Attr
	group *string
}

type sanitizingHandler struct {
	store      *LogStore
	redactor   *Redactor
	delegate   slog.Handler
	operations []handlerOperation
}

func newSanitizingHandler(store *LogStore, redactor *Redactor, delegate slog.Handler) *sanitizingHandler {
	if redactor == nil {
		redactor = NewRedactor(nil, nil)
	}
	if delegate == nil {
		delegate = slog.NewJSONHandler(io.Discard, nil)
	}
	if store == nil {
		store = newLogStore(nil, io.Discard)
	}
	return &sanitizingHandler{store: store, redactor: redactor, delegate: delegate}
}

func (h *sanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.delegate.Enabled(ctx, level)
}

func (h *sanitizingHandler) Handle(ctx context.Context, record slog.Record) error {
	state := sanitizer{
		snapshot: h.redactor.snapshot(),
		seen:     make(map[visit]struct{}),
	}
	message := state.cleanString(record.Message)

	var buffer bytes.Buffer
	var output slog.Handler = slog.NewJSONHandler(&buffer, nil)
	sensitiveBranch := false
	for _, operation := range h.operations {
		if operation.group != nil {
			if sensitiveBranch {
				continue
			}
			groupName := state.cleanString(*operation.group)
			if state.sensitiveKey(*operation.group) {
				output = output.WithAttrs([]slog.Attr{slog.String(groupName, state.snapshot.redactedMarker)})
				sensitiveBranch = true
				continue
			}
			output = output.WithGroup(groupName)
			continue
		}
		if sensitiveBranch {
			continue
		}
		attrs := sanitizeAttrs(&state, operation.attrs, 0)
		if len(attrs) > 0 {
			output = output.WithAttrs(attrs)
		}
	}

	sanitizedRecord := slog.NewRecord(record.Time, record.Level, message, record.PC)
	if !sensitiveBranch {
		record.Attrs(func(attr slog.Attr) bool {
			if sanitized, keep := sanitizeAttr(&state, attr, 0); keep {
				sanitizedRecord.AddAttrs(sanitized)
			}
			return true
		})
	}
	if err := output.Handle(ctx, sanitizedRecord); err != nil {
		return err
	}

	fields := make(map[string]any)
	if err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &fields); err != nil {
		return err
	}
	delete(fields, slog.TimeKey)
	delete(fields, slog.LevelKey)
	delete(fields, slog.MessageKey)
	logRecord := LogRecord{
		Time:    record.Time,
		Level:   record.Level.String(),
		Message: message,
		Fields:  fields,
	}
	h.store.append(logRecord, append([]byte(nil), buffer.Bytes()...))
	return nil
}

func (h *sanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := h.clone()
	clone.operations = append(clone.operations, handlerOperation{attrs: copyAttrs(attrs)})
	return clone
}

func (h *sanitizingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := h.clone()
	group := string(append([]byte(nil), name...))
	clone.operations = append(clone.operations, handlerOperation{group: &group})
	return clone
}

func (h *sanitizingHandler) clone() *sanitizingHandler {
	operations := make([]handlerOperation, len(h.operations))
	for index, operation := range h.operations {
		if operation.group != nil {
			group := *operation.group
			operations[index].group = &group
		} else {
			operations[index].attrs = copyAttrs(operation.attrs)
		}
	}
	return &sanitizingHandler{
		store:      h.store,
		redactor:   h.redactor,
		delegate:   h.delegate,
		operations: operations,
	}
}

func copyAttrs(attrs []slog.Attr) []slog.Attr {
	result := make([]slog.Attr, len(attrs))
	for index, attr := range attrs {
		result[index] = attr
		if attr.Value.Kind() == slog.KindGroup {
			result[index].Value = slog.GroupValue(copyAttrs(attr.Value.Group())...)
		}
	}
	return result
}

func sanitizeAttrs(state *sanitizer, attrs []slog.Attr, depth int) []slog.Attr {
	result := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		if state.elements >= maxSanitizedElements {
			result = append(result, slog.String(state.snapshot.unsafeMarker, state.snapshot.unsafeMarker))
			break
		}
		if sanitized, keep := sanitizeAttr(state, attr, depth); keep {
			result = append(result, sanitized)
		}
	}
	return result
}

func sanitizeAttr(state *sanitizer, attr slog.Attr, depth int) (slog.Attr, bool) {
	if depth > maxSanitizedDepth || state.elements >= maxSanitizedElements {
		return slog.String(state.snapshot.unsafeMarker, state.snapshot.unsafeMarker), true
	}
	state.elements++
	key := state.cleanString(attr.Key)
	if state.sensitiveKey(attr.Key) {
		return slog.String(key, state.snapshot.redactedMarker), true
	}
	value, ok := resolveSlogValue(attr.Value)
	if !ok {
		return slog.String(key, state.snapshot.unsafeMarker), true
	}
	switch value.Kind() {
	case slog.KindAny:
		return slog.Any(key, state.boundComposite(state.value(reflect.ValueOf(value.Any()), depth+1))), true
	case slog.KindBool:
		return slog.Bool(key, value.Bool()), true
	case slog.KindDuration:
		return slog.Duration(key, value.Duration()), true
	case slog.KindFloat64:
		return slog.Float64(key, value.Float64()), true
	case slog.KindInt64:
		return slog.Int64(key, value.Int64()), true
	case slog.KindString:
		return slog.String(key, state.cleanString(value.String())), true
	case slog.KindTime:
		return slog.Time(key, value.Time()), true
	case slog.KindUint64:
		return slog.Uint64(key, value.Uint64()), true
	case slog.KindGroup:
		children := sanitizeAttrs(state, value.Group(), depth+1)
		if len(children) == 0 {
			return slog.Attr{}, false
		}
		group := slog.Attr{Key: key, Value: slog.GroupValue(children...)}
		if encodedAttrSize(group) > maxSanitizedBytes {
			return slog.String(key, state.snapshot.truncationMarker), true
		}
		return group, true
	default:
		return slog.String(key, state.snapshot.unsafeMarker), true
	}
}

func encodedAttrSize(attr slog.Attr) int {
	var buffer bytes.Buffer
	handler := slog.NewJSONHandler(&buffer, nil)
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "", 0)
	record.AddAttrs(attr)
	if err := handler.Handle(context.Background(), record); err != nil {
		return maxSanitizedBytes + 1
	}
	return buffer.Len()
}
