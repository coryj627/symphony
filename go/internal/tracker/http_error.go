package tracker

import (
	"strings"
	"time"
	"unicode/utf8"
)

type Category string

const (
	// CategoryConfig maps unsupported and invalid tracker configuration.
	CategoryConfig Category = "tracker_config"
	// CategoryAuth maps missing credentials and provider authentication failures.
	CategoryAuth Category = "tracker_auth"
	// CategoryTransport maps failures before an HTTP response is available.
	CategoryTransport Category = "tracker_transport"
	// CategoryResponse maps non-success HTTP status responses.
	CategoryResponse Category = "tracker_response"
	// CategoryPayload maps malformed or semantically invalid provider payloads.
	CategoryPayload Category = "tracker_payload"
	// CategoryPagination maps incomplete, repeated, or malformed paging state.
	CategoryPagination Category = "tracker_pagination"
	// CategoryRateLimited maps provider rate limiting and retry metadata.
	CategoryRateLimited Category = "tracker_rate_limited"
	// CategoryScope maps a configured provider repository or project that is
	// missing or inaccessible to the selected credential.
	CategoryScope Category = "tracker_scope"
)

const maxPortableErrorBytes = 1024

type Error struct {
	Category   Category
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Status     int
}

// Error renders only the stable category and already-redacted operator text.
// HTTP bodies, headers, URLs, credentials, and diagnostic payloads do not have
// fields on this portable error and must use the centralized redactor instead.
func (err Error) Error() string {
	category := string(err.Category)
	if category == "" {
		category = "tracker_error"
	}
	message := strings.TrimSpace(err.Message)
	if message == "" {
		return boundPortableError(category)
	}
	return boundPortableError(category + ": " + message)
}

func boundPortableError(message string) string {
	message = strings.ToValidUTF8(message, "�")
	if len(message) <= maxPortableErrorBytes {
		return message
	}
	const suffix = "..."
	cut := maxPortableErrorBytes - len(suffix)
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + suffix
}
