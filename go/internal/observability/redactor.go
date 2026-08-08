package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	redactedMarker       = "[REDACTED]"
	unsafeValueMarker    = "[UNSAFE VALUE]"
	truncationMarker     = "…[TRUNCATED]"
	maxSanitizedBytes    = 64 << 10
	maxSanitizedDepth    = 32
	maxSanitizedElements = 4096
)

var (
	embeddedURLPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{0,31}://[^\s"'<>]+`)
	bearerPattern      = regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]{16,}`)
	githubTokenPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	openAITokenPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)
	linearTokenPattern = regexp.MustCompile(`\blin_api_[A-Za-z0-9_-]{16,}\b`)
	jwtPattern         = regexp.MustCompile(`\b[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
)

// LookupEnv resolves a declared environment-variable name.
type LookupEnv func(string) (string, bool)

// Redactor owns a race-safe, append-only collection of sensitive names and
// exact secret values. Values are copied on registration and on each snapshot.
type Redactor struct {
	mu               sync.RWMutex
	secrets          map[string]struct{}
	environmentNames map[string]struct{}
}

// NewRedactor creates a redactor and registers the supplied environment names.
func NewRedactor(secretEnvironmentNames []string, lookup LookupEnv) *Redactor {
	redactor := &Redactor{
		secrets:          make(map[string]struct{}),
		environmentNames: make(map[string]struct{}),
	}
	redactor.RegisterEnvironmentNames(secretEnvironmentNames, lookup)
	return redactor
}

// RegisterSecret copies and registers a non-empty exact secret value.
func (r *Redactor) RegisterSecret(secret []byte) {
	if r == nil || len(secret) == 0 {
		return
	}
	value := string(append([]byte(nil), secret...))
	if value == "" {
		return
	}
	r.mu.Lock()
	if r.secrets == nil {
		r.secrets = make(map[string]struct{})
	}
	r.secrets[value] = struct{}{}
	r.mu.Unlock()
}

// RegisterEnvironmentNames adds sensitive keys and resolves/registers their
// current non-empty values. Existing registrations remain protected.
func (r *Redactor) RegisterEnvironmentNames(names []string, lookup LookupEnv) {
	if r == nil {
		return
	}
	for _, supplied := range append([]string(nil), names...) {
		name := normalizeEnvironmentName(supplied)
		if name == "" {
			continue
		}
		r.mu.Lock()
		if r.environmentNames == nil {
			r.environmentNames = make(map[string]struct{})
		}
		r.environmentNames[strings.ToLower(name)] = struct{}{}
		r.mu.Unlock()

		if lookup == nil {
			continue
		}
		value, ok := safeLookup(lookup, name)
		if ok && value != "" {
			r.RegisterSecret([]byte(value))
		}
	}
}

// Value recursively returns a JSON-safe, bounded, sanitized copy of value.
func (r *Redactor) Value(value any) any {
	if r == nil {
		r = NewRedactor(nil, nil)
	}
	state := sanitizer{
		snapshot: r.snapshot(),
		seen:     make(map[visit]struct{}),
	}
	return state.boundComposite(state.value(reflect.ValueOf(value), 0))
}

type redactorSnapshot struct {
	secrets          []string
	environmentNames map[string]struct{}
	redactedMarker   string
	unsafeMarker     string
	truncationMarker string
}

func (r *Redactor) snapshot() redactorSnapshot {
	r.mu.RLock()
	snapshot := redactorSnapshot{
		secrets:          make([]string, 0, len(r.secrets)),
		environmentNames: make(map[string]struct{}, len(r.environmentNames)),
	}
	for secret := range r.secrets {
		snapshot.secrets = append(snapshot.secrets, secret)
	}
	for name := range r.environmentNames {
		snapshot.environmentNames[name] = struct{}{}
	}
	r.mu.RUnlock()
	sort.Slice(snapshot.secrets, func(left, right int) bool {
		return len(snapshot.secrets[left]) > len(snapshot.secrets[right])
	})
	snapshot.redactedMarker = safeStaticLiteral(redactedMarker, snapshot.secrets)
	snapshot.unsafeMarker = safeStaticLiteral(unsafeValueMarker, snapshot.secrets)
	snapshot.truncationMarker = safeStaticLiteral(truncationMarker, snapshot.secrets)
	return snapshot
}

func safeStaticLiteral(literal string, secrets []string) string {
	for {
		original := literal
		for _, secret := range secrets {
			if secret != "" {
				literal = strings.ReplaceAll(literal, secret, "")
			}
		}
		literal = strings.ToValidUTF8(literal, "")
		if literal == original {
			return literal
		}
	}
}

func normalizeEnvironmentName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "$")
}

func safeLookup(lookup LookupEnv, name string) (value string, ok bool) {
	defer func() {
		if recover() != nil {
			value, ok = "", false
		}
	}()
	return lookup(name)
}

type visit struct {
	typ reflect.Type
	ptr uintptr
}

type sanitizer struct {
	snapshot redactorSnapshot
	seen     map[visit]struct{}
	elements int
}

func (s *sanitizer) value(value reflect.Value, depth int) any {
	if depth > maxSanitizedDepth || s.elements >= maxSanitizedElements {
		return s.snapshot.unsafeMarker
	}
	if !value.IsValid() {
		return nil
	}
	s.elements++

	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return s.value(value.Elem(), depth+1)
	}

	if value.CanInterface() {
		original := value.Interface()
		switch typed := original.(type) {
		case slog.Value:
			return s.slogValue(typed, depth+1)
		case time.Time:
			return typed
		case time.Duration:
			return typed
		case url.URL:
			return s.cleanURL(&typed)
		case *url.URL:
			if typed == nil {
				return nil
			}
			return s.cleanURL(typed)
		case http.Header:
			return s.header(typed, depth+1)
		case []byte:
			return s.cleanString(string(append([]byte(nil), typed...)))
		case error:
			return s.cleanString(safeError(typed))
		case slog.LogValuer:
			resolved, ok := safeLogValue(typed)
			if !ok {
				return s.snapshot.unsafeMarker
			}
			return s.slogValue(resolved, depth+1)
		case fmt.Stringer:
			text, ok := safeString(typed)
			if !ok {
				return s.snapshot.unsafeMarker
			}
			return s.cleanString(text)
		}
	}

	switch value.Kind() {
	case reflect.Bool:
		return value.Bool()
	case reflect.String:
		return s.cleanString(value.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint()
	case reflect.Float32, reflect.Float64:
		return value.Float()
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		if !s.enter(value) {
			return s.snapshot.unsafeMarker
		}
		defer s.leave(value)
		return s.value(value.Elem(), depth+1)
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		if !s.enter(value) {
			return s.snapshot.unsafeMarker
		}
		defer s.leave(value)
		if value.Type().Key().Kind() != reflect.String {
			return s.snapshot.unsafeMarker
		}
		result := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			if s.elements >= maxSanitizedElements {
				result[s.snapshot.unsafeMarker] = s.snapshot.unsafeMarker
				break
			}
			originalKey := iterator.Key().String()
			key := s.cleanString(originalKey)
			if s.sensitiveKey(originalKey) {
				result[key] = s.snapshot.redactedMarker
				continue
			}
			result[key] = s.boundComposite(s.value(iterator.Value(), depth+1))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return nil
		}
		if !s.enter(value) {
			return s.snapshot.unsafeMarker
		}
		defer s.leave(value)
		return s.sequence(value, depth+1)
	case reflect.Array:
		return s.sequence(value, depth+1)
	case reflect.Invalid:
		return nil
	default:
		return s.snapshot.unsafeMarker
	}
}

func (s *sanitizer) sequence(value reflect.Value, depth int) any {
	result := make([]any, 0, min(value.Len(), maxSanitizedElements-s.elements))
	for index := 0; index < value.Len(); index++ {
		if s.elements >= maxSanitizedElements {
			result = append(result, s.snapshot.unsafeMarker)
			break
		}
		result = append(result, s.boundComposite(s.value(value.Index(index), depth)))
	}
	return result
}

func (s *sanitizer) enter(value reflect.Value) bool {
	pointer := value.Pointer()
	if pointer == 0 {
		return true
	}
	key := visit{typ: value.Type(), ptr: pointer}
	if _, exists := s.seen[key]; exists {
		return false
	}
	s.seen[key] = struct{}{}
	return true
}

func (s *sanitizer) leave(value reflect.Value) {
	pointer := value.Pointer()
	if pointer != 0 {
		delete(s.seen, visit{typ: value.Type(), ptr: pointer})
	}
}

func (s *sanitizer) header(header http.Header, depth int) map[string]any {
	result := make(map[string]any, len(header))
	for originalKey, sourceValues := range header {
		key := s.cleanString(originalKey)
		if s.sensitiveKey(originalKey) {
			result[key] = s.snapshot.redactedMarker
			continue
		}
		values := make([]any, 0, len(sourceValues))
		for _, value := range append([]string(nil), sourceValues...) {
			values = append(values, s.cleanString(value))
		}
		result[key] = s.boundComposite(values)
	}
	return result
}

func (s *sanitizer) slogValue(value slog.Value, depth int) any {
	resolved, ok := resolveSlogValue(value)
	if !ok {
		return s.snapshot.unsafeMarker
	}
	switch resolved.Kind() {
	case slog.KindAny:
		return s.value(reflect.ValueOf(resolved.Any()), depth+1)
	case slog.KindBool:
		return resolved.Bool()
	case slog.KindDuration:
		return resolved.Duration()
	case slog.KindFloat64:
		return resolved.Float64()
	case slog.KindInt64:
		return resolved.Int64()
	case slog.KindString:
		return s.cleanString(resolved.String())
	case slog.KindTime:
		return resolved.Time()
	case slog.KindUint64:
		return resolved.Uint64()
	case slog.KindGroup:
		group := make(map[string]any)
		for _, attr := range resolved.Group() {
			key := s.cleanString(attr.Key)
			if s.sensitiveKey(attr.Key) {
				group[key] = s.snapshot.redactedMarker
				continue
			}
			group[key] = s.boundComposite(s.slogValue(attr.Value, depth+1))
		}
		return group
	default:
		return s.snapshot.unsafeMarker
	}
}

func (s *sanitizer) cleanURL(source *url.URL) string {
	if source == nil {
		return ""
	}
	copyURL := *source
	copyURL.User = nil
	copyURL.Fragment = ""
	if copyURL.Opaque != "" {
		copyURL.Opaque = s.snapshot.redactedMarker
	}
	query := copyURL.Query()
	for key := range query {
		if sensitiveURLKey(key) {
			query[key] = []string{s.snapshot.redactedMarker}
		}
	}
	copyURL.RawQuery = query.Encode()
	return s.cleanStringNoURLs(copyURL.String())
}

func (s *sanitizer) cleanString(value string) string {
	value = embeddedURLPattern.ReplaceAllStringFunc(value, func(candidate string) string {
		trailing := ""
		for len(candidate) > 0 {
			last := candidate[len(candidate)-1]
			if !strings.ContainsRune(".,;:!?)]}", rune(last)) {
				break
			}
			trailing = string(last) + trailing
			candidate = candidate[:len(candidate)-1]
		}
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return candidate + trailing
		}
		return s.cleanURL(parsed) + trailing
	})
	return s.cleanStringNoURLs(value)
}

func (s *sanitizer) cleanStringNoURLs(value string) string {
	value = s.redactCredentialText(value)
	value = stripANSIAndControls(value)
	value = strings.ToValidUTF8(value, "�")
	// Removing controls can join credential fragments. Redact a second time so
	// sanitization itself can never reconstruct a secret.
	value = s.redactCredentialText(value)
	return truncateUTF8(value, s.snapshot.truncationMarker)
}

func (s *sanitizer) redactCredentialText(value string) string {
	for _, secret := range s.snapshot.secrets {
		value = strings.ReplaceAll(value, secret, s.snapshot.redactedMarker)
	}
	value = bearerPattern.ReplaceAllStringFunc(value, func(match string) string {
		space := strings.IndexAny(match, " \t")
		if space < 0 {
			return s.snapshot.redactedMarker
		}
		return match[:space] + " " + s.snapshot.redactedMarker
	})
	value = githubTokenPattern.ReplaceAllString(value, s.snapshot.redactedMarker)
	value = openAITokenPattern.ReplaceAllString(value, s.snapshot.redactedMarker)
	value = linearTokenPattern.ReplaceAllString(value, s.snapshot.redactedMarker)
	value = jwtPattern.ReplaceAllString(value, s.snapshot.redactedMarker)
	return value
}

func (s *sanitizer) sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(stripANSIAndControls(key), "$")))
	if _, ok := s.snapshot.environmentNames[normalized]; ok {
		return true
	}
	switch normalized {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	default:
		return sensitiveURLKey(normalized)
	}
}

func sensitiveURLKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(stripANSIAndControls(key)))
	if normalized == "code" {
		return true
	}
	for _, fragment := range []string{
		"token", "secret", "password", "passwd", "api_key", "apikey",
		"authorization", "auth", "session", "cookie", "csrf", "bootstrap", "capability",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func safeError(err error) string {
	return safeErrorDepth(err, 0)
}

func safeErrorDepth(err error, depth int) (result string) {
	if err == nil {
		return ""
	}
	if depth > maxSanitizedDepth {
		return unsafeValueMarker
	}
	defer func() {
		if recover() != nil {
			parts := make([]string, 0, 2)
			switch wrapped := err.(type) {
			case interface{ Unwrap() []error }:
				children, ok := safeUnwrapMany(wrapped)
				if !ok {
					result = unsafeValueMarker
					return
				}
				for _, child := range children {
					parts = append(parts, safeErrorDepth(child, depth+1))
				}
			case interface{ Unwrap() error }:
				child, ok := safeUnwrapOne(wrapped)
				if !ok {
					result = unsafeValueMarker
					return
				}
				parts = append(parts, safeErrorDepth(child, depth+1))
			}
			if len(parts) == 0 {
				result = unsafeValueMarker
			} else {
				result = strings.Join(parts, ": ")
			}
		}
	}()
	return err.Error()
}

func safeUnwrapMany(value interface{ Unwrap() []error }) (children []error, ok bool) {
	defer func() {
		if recover() != nil {
			children, ok = nil, false
		}
	}()
	return value.Unwrap(), true
}

func safeUnwrapOne(value interface{ Unwrap() error }) (child error, ok bool) {
	defer func() {
		if recover() != nil {
			child, ok = nil, false
		}
	}()
	return value.Unwrap(), true
}

func safeString(value fmt.Stringer) (result string, ok bool) {
	defer func() {
		if recover() != nil {
			result, ok = unsafeValueMarker, false
		}
	}()
	return value.String(), true
}

func safeLogValue(value slog.LogValuer) (result slog.Value, ok bool) {
	defer func() {
		if recover() != nil {
			result, ok = slog.StringValue(unsafeValueMarker), false
		}
	}()
	return value.LogValue(), true
}

func resolveSlogValue(value slog.Value) (result slog.Value, ok bool) {
	result = value
	for attempts := 0; attempts < maxSanitizedDepth && result.Kind() == slog.KindLogValuer; attempts++ {
		valuer, valid := result.Any().(slog.LogValuer)
		if !valid {
			return slog.StringValue(unsafeValueMarker), false
		}
		var resolved slog.Value
		resolved, valid = safeLogValue(valuer)
		if !valid {
			return slog.StringValue(unsafeValueMarker), false
		}
		result = resolved
	}
	if result.Kind() == slog.KindLogValuer {
		return slog.StringValue(unsafeValueMarker), false
	}
	return result, true
}

func stripANSIAndControls(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] == 0x1b {
			index++
			if index >= len(value) {
				break
			}
			switch value[index] {
			case '[':
				index++
				for index < len(value) {
					current := value[index]
					index++
					if current >= 0x40 && current <= 0x7e {
						break
					}
				}
				continue
			case ']':
				index++
				for index < len(value) {
					if value[index] == 0x07 {
						index++
						break
					}
					if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
						index += 2
						break
					}
					index++
				}
				continue
			default:
				continue
			}
		}
		if value[index] >= 0x80 && value[index] <= 0x9f {
			index++
			continue
		}
		runeValue, size := utf8.DecodeRuneInString(value[index:])
		if runeValue == utf8.RuneError && size == 1 {
			builder.WriteRune(utf8.RuneError)
			index++
			continue
		}
		index += size
		if runeValue == '\t' || runeValue == '\n' {
			builder.WriteRune(runeValue)
			continue
		}
		if runeValue < 0x20 || (runeValue >= 0x7f && runeValue <= 0x9f) {
			continue
		}
		builder.WriteRune(runeValue)
	}
	return builder.String()
}

func truncateUTF8(value, marker string) string {
	if len(value) <= maxSanitizedBytes {
		return value
	}
	if len(marker) > maxSanitizedBytes {
		marker = ""
	}
	limit := maxSanitizedBytes - len(marker)
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + marker
}

func (s *sanitizer) boundComposite(value any) any {
	if value == nil {
		return nil
	}
	if _, scalar := value.(string); scalar {
		return value
	}
	data, err := safeJSONMarshal(value)
	if err != nil {
		return s.snapshot.unsafeMarker
	}
	if len(data) <= maxSanitizedBytes {
		return value
	}
	return s.snapshot.truncationMarker
}

func safeJSONMarshal(value any) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			data, err = nil, errors.New("unsafe JSON value")
		}
	}()
	return json.Marshal(value)
}

func toJSON(value any) string {
	data, err := safeJSONMarshal(value)
	if err != nil {
		return `"[UNSAFE VALUE]"`
	}
	return string(data)
}
