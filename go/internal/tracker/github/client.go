package github

import (
	"context"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/tracker"
)

const (
	requestTimeout       = 30 * time.Second
	maxRedirects         = 10
	maxResponseBodyBytes = 16 << 20
	maxRetryAfter        = 24 * time.Hour
	githubUserAgent      = "symphony-go/github"
)

var errRedirectRejected = errors.New("GitHub redirect rejected")

type apiOrigin struct {
	hostname string
	port     string
}

type httpResult struct {
	status int
	header http.Header
	body   []byte
}

func parseEndpoint(raw string) (*url.URL, apiOrigin, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, apiOrigin{}, configError("GitHub endpoint must be an HTTPS URL without userinfo, query, or fragment")
	}
	origin := originForURL(parsed)
	if origin.hostname == "" {
		return nil, apiOrigin{}, configError("GitHub endpoint must include a host")
	}
	return parsed, origin, nil
}

func originForURL(value *url.URL) apiOrigin {
	port := value.Port()
	if port == "" && strings.EqualFold(value.Scheme, "https") {
		port = "443"
	}
	return apiOrigin{hostname: strings.ToLower(value.Hostname()), port: port}
}

func sameHTTPSOrigin(value *url.URL, allowed apiOrigin) bool {
	return value != nil && value.User == nil && strings.EqualFold(value.Scheme, "https") && originForURL(value) == allowed
}

func cloneHTTPClient(client *http.Client, allowed apiOrigin) *http.Client {
	clone := &http.Client{}
	if client != nil {
		*clone = *client
	}
	clone.Jar = nil
	clone.Timeout = requestTimeout
	clone.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errRedirectRejected
		}
		if !sameHTTPSOrigin(request.URL, allowed) {
			return errRedirectRejected
		}
		return nil
	}
	return clone
}

func appendEscapedPath(base *url.URL, segments ...string) (*url.URL, error) {
	escapedPath := strings.TrimSuffix(base.EscapedPath(), "/")
	for _, segment := range segments {
		escapedPath += "/" + url.PathEscape(segment)
	}
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, configError("GitHub repository scope is invalid")
	}
	result := *base
	result.Path = decodedPath
	result.RawPath = escapedPath
	result.RawQuery = ""
	result.ForceQuery = false
	result.Fragment = ""
	return &result, nil
}

func (adapter *Adapter) request(ctx context.Context, requestURL *url.URL, etag string, allowNotModified, allowNotFound bool) (httpResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return httpResult{}, transportError(ctx, "GitHub request could not be created")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("Authorization", adapter.authorization())
	request.Header.Set("User-Agent", githubUserAgent)
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}

	response, err := adapter.client.Do(request)
	if err != nil {
		return httpResult{}, transportError(ctx, "GitHub request failed")
	}
	defer response.Body.Close()
	result := httpResult{status: response.StatusCode, header: response.Header.Clone()}
	if response.StatusCode == http.StatusNotModified && allowNotModified {
		return result, nil
	}
	if response.StatusCode == http.StatusNotFound && allowNotFound {
		return result, nil
	}
	if response.StatusCode != http.StatusOK {
		return httpResult{}, statusError(response.StatusCode, response.Header)
	}
	if response.ContentLength > maxResponseBodyBytes {
		return httpResult{}, payloadError("GitHub response exceeded the size limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyBytes+1))
	if err != nil {
		return httpResult{}, transportError(ctx, "GitHub response could not be read")
	}
	if len(body) > maxResponseBodyBytes {
		return httpResult{}, payloadError("GitHub response exceeded the size limit")
	}
	result.body = body
	return result, nil
}

func statusError(status int, header http.Header) error {
	switch status {
	case http.StatusUnauthorized:
		return &tracker.Error{Category: tracker.CategoryAuth, Message: "GitHub authentication failed", Status: status}
	case http.StatusNotFound:
		return &tracker.Error{Category: tracker.CategoryScope, Message: "GitHub repository is missing or inaccessible", Status: status}
	case http.StatusForbidden:
		if isRateLimited(header) {
			return &tracker.Error{
				Category: tracker.CategoryRateLimited, Message: "GitHub rate limit was reached",
				Retryable: true, RetryAfter: retryAfter(header, time.Now()), Status: status,
			}
		}
		return &tracker.Error{Category: tracker.CategoryAuth, Message: "GitHub authorization failed", Status: status}
	case http.StatusTooManyRequests:
		return &tracker.Error{
			Category: tracker.CategoryRateLimited, Message: "GitHub rate limit was reached",
			Retryable: true, RetryAfter: retryAfter(header, time.Now()), Status: status,
		}
	default:
		return &tracker.Error{
			Category: tracker.CategoryResponse, Message: "GitHub returned an unexpected status",
			Retryable: status == http.StatusRequestTimeout || status >= 500, Status: status,
		}
	}
}

func isRateLimited(header http.Header) bool {
	return strings.TrimSpace(header.Get("Retry-After")) != "" || strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0"
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
		if seconds, ok := boundedSeconds(value); ok {
			return seconds
		}
		if deadline, err := http.ParseTime(value); err == nil {
			return boundRetryAfter(deadline.Sub(now))
		}
	}
	if value := strings.TrimSpace(header.Get("X-RateLimit-Reset")); value != "" {
		if unixSeconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			return boundRetryAfter(time.Unix(unixSeconds, 0).Sub(now))
		}
	}
	return 0
}

func boundedSeconds(value string) (time.Duration, bool) {
	integer := new(big.Int)
	if _, ok := integer.SetString(value, 10); !ok || integer.Sign() < 0 {
		return 0, false
	}
	maximum := big.NewInt(int64(maxRetryAfter / time.Second))
	if integer.Cmp(maximum) > 0 {
		return maxRetryAfter, true
	}
	return time.Duration(integer.Int64()) * time.Second, true
}

func boundRetryAfter(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	if value > maxRetryAfter {
		return maxRetryAfter
	}
	return value
}

func configError(message string) error {
	return &tracker.Error{Category: tracker.CategoryConfig, Message: message}
}

func authError(message string) error {
	return &tracker.Error{Category: tracker.CategoryAuth, Message: message}
}

func transportError(ctx context.Context, message string) error {
	retryable := ctx == nil || ctx.Err() == nil
	return &tracker.Error{Category: tracker.CategoryTransport, Message: message, Retryable: retryable}
}

func payloadError(message string) error {
	return &tracker.Error{Category: tracker.CategoryPayload, Message: message}
}

func paginationError(message string) error {
	return &tracker.Error{Category: tracker.CategoryPagination, Message: message}
}
