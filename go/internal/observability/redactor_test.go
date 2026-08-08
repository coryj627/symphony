package observability

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const testCanary = "canary-token-123456789"

func TestRedactorSanitizesNestedValuesWithoutMutatingCallers(t *testing.T) {
	t.Parallel()

	header := http.Header{
		"Authorization": {"Bearer " + testCanary},
		"X-Diagnostic":  {"safe", testCanary},
	}
	nested := map[string]any{
		"header": header,
		"slice":  []any{"safe", testCanary},
		"bytes":  []byte("prefix " + testCanary),
		"group": slog.GroupValue(
			slog.String("safe", "visible"),
			slog.String("Cookie", "session="+testCanary),
		),
	}

	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(testCanary))
	got := redactor.Value(nested)

	assertNoCanary(t, got)
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("Value() type = %T, want map[string]any", got)
	}
	if gotMap["bytes"] != "prefix [REDACTED]" {
		t.Fatal("byte buffer was not replaced with the redacted diagnostic form")
	}
	if header.Get("Authorization") != "Bearer "+testCanary {
		t.Fatal("caller header was mutated")
	}
	if nested["slice"].([]any)[1] != testCanary {
		t.Fatal("caller slice was mutated")
	}
}

func TestRedactorRegistersEnvironmentNamesAndOverlappingSecrets(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"LINEAR_API_KEY": "lin_api_abcdefghijklmnopqrstuvwxyz",
		"NEW_SECRET":     "overlap-secret-long",
	}
	lookup := func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
	redactor := NewRedactor([]string{"LINEAR_API_KEY", "MISSING"}, lookup)
	redactor.RegisterSecret(nil)
	redactor.RegisterSecret([]byte("overlap-secret"))
	redactor.RegisterEnvironmentNames([]string{"new_secret", "NEW_SECRET"}, func(name string) (string, bool) {
		return values[strings.ToUpper(name)], true
	})

	got := redactor.Value(map[string]any{
		"linear_api_key": values["LINEAR_API_KEY"],
		"NEW_SECRET":     values["NEW_SECRET"],
		"message":        values["NEW_SECRET"] + " / overlap-secret",
	})
	assertNoCanaryString(t, safeSprint(got), values["LINEAR_API_KEY"])
	assertNoCanaryString(t, safeSprint(got), values["NEW_SECRET"])
	assertNoCanaryString(t, safeSprint(got), "overlap-secret")
}

func TestRedactorMarkerNeverReintroducesARegisteredSecret(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte("REDACTED"))
	got := redactor.Value("REDACTED")
	assertNoCanaryString(t, safeSprint(got), "REDACTED")
}

func TestRedactorSanitizesURLsCredentialShapesAndControls(t *testing.T) {
	t.Parallel()

	typed, err := url.Parse("https://user:" + testCanary + "@example.test/path?access_token=" + testCanary + "&page=2#" + testCanary)
	if err != nil {
		t.Fatal(err)
	}
	typedBefore := typed.String()
	input := strings.Join([]string{
		"Bearer abcdefghijklmnopqrstuvwxyz",
		"github_pat_abcdefghijklmnopqrstuvwxyz0123456789",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"sk-abcdefghijklmnopqrstuvwxyz012345",
		"lin_api_abcdefghijklmnopqrstuvwxyz",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signaturevalue",
		"https://user:password@example.test/a?api_key=secret-value&safe=yes#capability",
		"\x1b[31mred\x1b[0m\x1b]0;title\x07\r\x7f\u0085ok",
		string([]byte{'a', 0xff, 'b'}),
	}, " ")

	redactor := NewRedactor(nil, nil)
	got := redactor.Value(map[string]any{"message": input, "url": typed}).(map[string]any)
	if typed.String() != typedBefore {
		t.Fatal("caller URL was mutated")
	}
	text := safeSprint(got)
	for _, forbidden := range []string{
		"abcdefghijklmnopqrstuvwxyz", "password@", "secret-value", "#capability",
		"\x1b", "\r", "\x7f", "\u0085",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatal("sanitized value contains forbidden credential or control content")
		}
	}
	if !strings.Contains(text, "safe=yes") || !strings.Contains(text, "page=2") {
		t.Fatal("safe URL diagnostics were removed")
	}
	if !utf8.ValidString(text) {
		t.Fatal("invalid UTF-8 survived sanitization")
	}
}

func TestRedactorDoesNotTrustOpaqueTypedURLs(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	typed := &url.URL{Scheme: "custom", Opaque: "opaque-capability-value"}
	got := safeSprint(redactor.Value(typed))
	if strings.Contains(got, "opaque-capability-value") {
		t.Fatal("opaque typed URL capability survived sanitization")
	}
}

func TestRedactorSanitizesNonHTTPWholeURLStrings(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	got := safeSprint(redactor.Value("custom://user:password@example.test/path?token=short-value#fragment-value"))
	for _, forbidden := range []string{"password", "short-value", "fragment-value"} {
		if strings.Contains(got, forbidden) {
			t.Fatal("non-HTTP URL credential survived sanitization")
		}
	}
}

func TestRedactorResanitizesURLsReconstructedByControlRemoval(t *testing.T) {
	t.Parallel()

	reconstructed := "https:\x1b[31m//alice:ordinary-password@example.test/path?access_token=short-secret#capability-fragment"
	redactor := NewRedactor(nil, nil)
	for _, input := range []any{
		reconstructed,
		"before " + reconstructed + " after",
		map[string]any{"value": reconstructed},
		map[string]any{reconstructed: "safe"},
	} {
		assertURLCredentialsAbsent(t, safeSprint(redactor.Value(input)))
	}
}

func TestRedactorSanitizesAbsoluteMalformedAndEncodedURLs(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte("ordinary/credential"))
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "opaque absolute URL",
			input: "custom:opaque-capability?access_token=short-secret#capability-fragment",
		},
		{
			name:  "malformed credential URL",
			input: "https://alice:ordinary-password@[::1/path?access_token=short-secret#capability-fragment",
		},
		{
			name:  "encoded credential query value",
			input: "https://example.test/docs/readme?next=ordinary%2Fcredential&safe=yes",
		},
		{
			name:  "double encoded credential query value",
			input: "https://example.test/docs/readme?next=ordinary%252Fcredential&safe=yes",
		},
		{
			name:  "encoded credential path",
			input: "https://example.test/files/ordinary%2Fcredential?safe=yes",
		},
		{
			name:  "mixed case repeated sensitive values",
			input: "https://example.test/docs/readme?AcCeSs_ToKeN=one&AcCeSs_ToKeN=two&safe=yes",
		},
		{
			name:  "embedded encoded credential URL",
			input: "before https://example.test/docs/readme?next=ordinary%2Fcredential&safe=yes after",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := safeSprint(redactor.Value(test.input))
			assertURLCredentialsAbsent(t, got)
			if strings.Contains(strings.ToLower(got), "ordinary%2fcredential") ||
				strings.Contains(strings.ToLower(got), "ordinary%252fcredential") ||
				strings.Contains(got, "ordinary/credential") {
				t.Fatal("reversibly encoded credential survived URL sanitization")
			}
		})
	}

	safe := safeSprint(redactor.Value("ordinary text https://example.test/docs/readme?safe=yes remains"))
	if !strings.Contains(safe, "/docs/readme") || !strings.Contains(safe, "safe=yes") || !strings.Contains(safe, "ordinary text") {
		t.Fatal("non-sensitive URL path or surrounding text was not preserved")
	}
}

func TestRedactorSanitizesEncodedTypedURLWithoutMutatingCaller(t *testing.T) {
	t.Parallel()

	typed, err := url.Parse("custom://example.test/files/ordinary%2Fcredential?next=ordinary%2Fcredential&safe=yes#capability-fragment")
	if err != nil {
		t.Fatal(err)
	}
	before := typed.String()
	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte("ordinary/credential"))
	got := safeSprint(redactor.Value(typed))
	if typed.String() != before {
		t.Fatal("typed URL caller value was mutated")
	}
	assertURLCredentialsAbsent(t, got)
	if strings.Contains(strings.ToLower(got), "ordinary%2fcredential") || strings.Contains(got, "ordinary/credential") {
		t.Fatal("typed URL retained a reversibly encoded credential")
	}
}

func TestRedactorDoesNotRevealSecretsWhenControlsAreRemoved(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(testCanary))
	split := "canary-token-\x1b[31m123456789"
	got := redactor.Value(split)
	assertNoCanary(t, got)
}

func TestRedactorRemovesRawC1ControlBytes(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	got := redactor.Value(string([]byte{'a', 0x80, 'b'}))
	if got != "ab" {
		t.Fatal("raw C1 control byte was not removed")
	}
}

func TestRedactorStripsComplete8BitC1ANSISequences(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "raw CSI terminated", input: string([]byte{0x9b}) + "31mred", want: "red"},
		{name: "raw CSI unterminated", input: "safe" + string([]byte{0x9b}) + "31", want: "safe"},
		{name: "raw OSC BEL", input: string([]byte{0x9d}) + "0;window-title" + string([]byte{0x07}) + "safe", want: "safe"},
		{name: "raw OSC ST", input: string([]byte{0x9d}) + "0;window-title" + string([]byte{0x9c}) + "safe", want: "safe"},
		{name: "raw OSC ESC ST", input: string([]byte{0x9d}) + "0;window-title\x1b\\safe", want: "safe"},
		{name: "raw OSC unterminated", input: "safe" + string([]byte{0x9d}) + "0;window-title", want: "safe"},
		{name: "UTF-8 CSI", input: "\u009b31mred", want: "red"},
		{name: "UTF-8 OSC", input: "\u009d0;window-title\u009csafe", want: "safe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := redactor.Value(test.input); got != test.want {
				t.Fatal("C1 ANSI sequence payload survived or safe text was removed")
			}
		})
	}
}

func TestRedactorTruncatesAfterRedactionAtUTF8Boundary(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(testCanary))
	input := strings.Repeat("é", maxSanitizedBytes) + testCanary
	got, ok := redactor.Value(input).(string)
	if !ok {
		t.Fatalf("Value() type = %T, want string", redactor.Value(input))
	}
	if len(got) > maxSanitizedBytes {
		t.Fatalf("sanitized length = %d, want <= %d", len(got), maxSanitizedBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated value is not valid UTF-8")
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Fatalf("truncated value lacks marker")
	}
	if strings.Contains(got, testCanary) {
		t.Fatal("secret survived truncation")
	}
}

func TestRedactorKeepsDynamicMarkersValidForBinarySecrets(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte{0xe2})
	got := redactor.Value(strings.Repeat("x", maxSanitizedBytes+1)).(string)
	if !utf8.ValidString(got) {
		t.Fatal("dynamic truncation marker is invalid UTF-8")
	}
}

type cyclicValue struct {
	Next *cyclicValue
}

type panicError struct{}

func (panicError) Error() string { panic("error panic") }

type panicStringer struct{}

func (panicStringer) String() string { panic("string panic") }

type panicLogValuer struct{}

func (panicLogValuer) LogValue() slog.Value { panic("log value panic") }

type panicMarshaler struct{}

func (panicMarshaler) MarshalJSON() ([]byte, error) { panic("marshal panic") }

type panicWrappedError struct {
	next error
}

func (err *panicWrappedError) Error() string { panic("wrapped error panic") }
func (err *panicWrappedError) Unwrap() error { return err.next }

type panicUnwrapError struct{}

func (panicUnwrapError) Error() string { panic("error panic") }
func (panicUnwrapError) Unwrap() error { panic("unwrap panic") }

func TestRedactorContainsCyclesBudgetsAndPanickingValues(t *testing.T) {
	t.Parallel()

	cycle := &cyclicValue{}
	cycle.Next = cycle
	joined := errors.Join(errors.New("outer "+testCanary), panicError{})
	tooMany := make([]any, maxSanitizedElements+5)
	for index := range tooMany {
		tooMany[index] = index
	}

	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(testCanary))
	got := redactor.Value(map[string]any{
		"cycle":     cycle,
		"error":     joined,
		"unwrap":    panicUnwrapError{},
		"stringer":  panicStringer{},
		"valuer":    panicLogValuer{},
		"marshaler": panicMarshaler{},
		"many":      tooMany,
		"duration":  time.Second,
		"time":      time.Unix(123, 0).UTC(),
	})
	text := safeSprint(got)
	assertNoCanary(t, got)
	if !strings.Contains(text, unsafeValueMarker) {
		t.Fatal("expected static unsafe marker")
	}
}

func TestRedactorBoundsPanickingErrorChains(t *testing.T) {
	t.Parallel()

	var chain error = errors.New("base " + testCanary)
	for index := 0; index < maxSanitizedDepth+5; index++ {
		chain = &panicWrappedError{next: chain}
	}
	redactor := NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(testCanary))
	got := redactor.Value(chain)
	assertNoCanary(t, got)
	if !strings.Contains(safeSprint(got), unsafeValueMarker) {
		t.Fatalf("over-depth error chain was not bounded")
	}
}

func assertNoCanary(t *testing.T, value any) {
	t.Helper()
	assertNoCanaryString(t, safeSprint(value), testCanary)
}

func assertNoCanaryString(t *testing.T, value, canary string) {
	t.Helper()
	if strings.Contains(value, canary) {
		t.Fatal("secret canary survived sanitization")
	}
}

func assertURLCredentialsAbsent(t *testing.T, value string) {
	t.Helper()
	for _, forbidden := range []string{
		"alice", "ordinary-password", "short-secret", "capability-fragment", "opaque-capability",
	} {
		if strings.Contains(value, forbidden) {
			t.Fatal("URL credential or capability survived sanitization")
		}
	}
}

func safeSprint(value any) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(toJSON(value)), "\n", " "), "\t", " "))
}
