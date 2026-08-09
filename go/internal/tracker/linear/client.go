package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/tracker"
)

const (
	requestTimeout           = 30 * time.Second
	maxResponseBodyBytes     = 4 << 20
	maxRateLimitRetryAfter   = 24 * time.Hour
	fallbackRateLimitBackoff = time.Minute
	linearUserAgent          = "symphony-go/linear"
)

var linearResetHeaders = []string{
	"X-RateLimit-Requests-Reset",
	"X-RateLimit-Endpoint-Requests-Reset",
	"X-RateLimit-Complexity-Reset",
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type issuePage struct {
	nodes       []json.RawMessage
	hasNextPage bool
	endCursor   *string
}

func parseEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, configError("Linear endpoint must be an HTTPS URL without userinfo, query, or fragment")
	}
	if strings.TrimSpace(parsed.Hostname()) == "" {
		return nil, configError("Linear endpoint must include a host")
	}
	return parsed, nil
}

func cloneHTTPClient(client *http.Client) *http.Client {
	clone := &http.Client{}
	if client != nil {
		*clone = *client
	}
	clone.Jar = nil
	clone.Timeout = requestTimeout
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return clone
}

func (adapter *Adapter) request(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, configError("Linear request variables were invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, transportError(ctx, "Linear request could not be created")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", string(adapter.token))
	request.Header.Set("User-Agent", linearUserAgent)

	response, err := adapter.client.Do(request)
	if err != nil {
		return nil, transportError(ctx, "Linear request failed")
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusUnauthorized:
		return nil, &tracker.Error{Category: tracker.CategoryAuth, Message: "Linear authentication failed", Status: response.StatusCode}
	case http.StatusForbidden:
		return nil, &tracker.Error{Category: tracker.CategoryAuth, Message: "Linear authorization failed", Status: response.StatusCode}
	case http.StatusTooManyRequests:
		return nil, rateLimitError(response.StatusCode, response.Header, time.Now())
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusBadRequest {
		return nil, statusError(response.StatusCode)
	}

	if response.ContentLength > maxResponseBodyBytes {
		return nil, payloadError("Linear response exceeded the size limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, transportError(ctx, "Linear response could not be read")
	}
	if len(body) > maxResponseBodyBytes {
		return nil, payloadError("Linear response exceeded the size limit")
	}

	envelope, validEnvelope := decodeGraphQLEnvelope(body)
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusBadRequest && validEnvelope && len(envelope.errors) > 0 {
			return nil, graphQLError(envelope.errors, response.StatusCode, response.Header, time.Now())
		}
		return nil, statusError(response.StatusCode)
	}
	if !validEnvelope {
		return nil, payloadError("Linear returned a malformed GraphQL envelope")
	}
	if len(envelope.errors) > 0 {
		return nil, graphQLError(envelope.errors, response.StatusCode, response.Header, time.Now())
	}
	return append(json.RawMessage(nil), envelope.data...), nil
}

func (adapter *Adapter) requestIssuePage(ctx context.Context, query string, variables map[string]any) (issuePage, error) {
	data, err := adapter.request(ctx, query, variables)
	if err != nil {
		return issuePage{}, err
	}
	page, ok := decodeIssuePage(data)
	if !ok {
		return issuePage{}, payloadError("Linear returned a malformed issue payload")
	}
	return page, nil
}

type decodedEnvelope struct {
	data   json.RawMessage
	errors []graphQLErrorRecord
}

type graphQLErrorRecord struct {
	Extensions json.RawMessage `json:"extensions"`
}

func decodeGraphQLEnvelope(body []byte) (decodedEnvelope, bool) {
	var raw struct {
		Data   json.RawMessage `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if !decodeOneJSON(body, &raw) {
		return decodedEnvelope{}, false
	}
	errorsList := []graphQLErrorRecord{}
	if len(raw.Errors) > 0 && string(bytes.TrimSpace(raw.Errors)) != "null" {
		if !decodeOneJSON(raw.Errors, &errorsList) || errorsList == nil {
			return decodedEnvelope{}, false
		}
	}
	if len(raw.Data) == 0 && len(errorsList) == 0 {
		return decodedEnvelope{}, false
	}
	return decodedEnvelope{data: raw.Data, errors: errorsList}, true
}

func decodeIssuePage(raw json.RawMessage) (issuePage, bool) {
	var data struct {
		Issues json.RawMessage `json:"issues"`
	}
	if !decodeOneJSON(raw, &data) {
		return issuePage{}, false
	}
	var connection struct {
		Nodes    json.RawMessage `json:"nodes"`
		PageInfo json.RawMessage `json:"pageInfo"`
	}
	if !decodeOneJSON(data.Issues, &connection) {
		return issuePage{}, false
	}
	var nodes []json.RawMessage
	if !decodeOneJSON(connection.Nodes, &nodes) || nodes == nil {
		return issuePage{}, false
	}
	var pageInfo rawPageInfo
	if !decodeOneJSON(connection.PageInfo, &pageInfo) {
		return issuePage{}, false
	}
	var hasNext bool
	if !decodeOneJSON(pageInfo.HasNextPage, &hasNext) {
		return issuePage{}, false
	}
	endCursor, ok := decodeEndCursor(pageInfo.EndCursor)
	if !ok {
		return issuePage{}, false
	}
	return issuePage{nodes: nodes, hasNextPage: hasNext, endCursor: endCursor}, true
}

func decodeEndCursor(raw json.RawMessage) (*string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, true
	}
	var cursor string
	if !decodeOneJSON(raw, &cursor) {
		return nil, false
	}
	return stringPointer(cursor), true
}

func graphQLError(records []graphQLErrorRecord, status int, header http.Header, now time.Time) error {
	category := tracker.CategoryPayload
	for _, record := range records {
		code := graphQLErrorCode(record.Extensions)
		if code == "RATELIMITED" {
			return rateLimitError(status, header, now)
		}
		if recognizedGraphQLAuthCode(code) {
			category = tracker.CategoryAuth
		}
	}
	if category == tracker.CategoryAuth {
		return &tracker.Error{Category: category, Message: "Linear GraphQL authorization failed", Status: status}
	}
	return &tracker.Error{Category: category, Message: "Linear GraphQL operation failed", Status: status}
}

func graphQLErrorCode(extensions json.RawMessage) string {
	code, ok := requiredNestedString(extensions, "code")
	if !ok {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(code))
}

func recognizedGraphQLAuthCode(code string) bool {
	switch code {
	case "AUTHENTICATION_ERROR", "AUTHENTICATION_REQUIRED", "UNAUTHENTICATED", "FORBIDDEN", "FORBIDDEN_ERROR", "PERMISSION_DENIED":
		return true
	default:
		return false
	}
}

func statusError(status int) error {
	return &tracker.Error{
		Category: tracker.CategoryResponse, Message: "Linear returned an unexpected HTTP status",
		Retryable: status == http.StatusRequestTimeout || status >= 500, Status: status,
	}
}

func rateLimitError(status int, header http.Header, now time.Time) error {
	return &tracker.Error{
		Category: tracker.CategoryRateLimited, Message: "Linear rate limit was reached",
		Retryable: true, RetryAfter: linearRetryAfter(header, now), Status: status,
	}
}

func linearRetryAfter(header http.Header, now time.Time) time.Duration {
	nowMilliseconds := now.UnixMilli()
	maxMilliseconds := int64(maxRateLimitRetryAfter / time.Millisecond)
	latest := int64(0)
	for _, name := range linearResetHeaders {
		for _, raw := range header.Values(name) {
			reset, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil || reset <= nowMilliseconds {
				continue
			}
			if reset > latest {
				latest = reset
			}
		}
	}
	if latest == 0 {
		return fallbackRateLimitBackoff
	}
	if latest > nowMilliseconds+maxMilliseconds {
		return maxRateLimitRetryAfter
	}
	return time.Duration(latest-nowMilliseconds) * time.Millisecond
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
