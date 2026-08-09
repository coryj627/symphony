package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/domain"
)

const (
	maximumIdentifierBytes = 256
	maximumShortTextBytes  = 128
	maximumDisplayBytes    = 512
	maximumDescription     = 16 << 10
)

type apiErrorResponse struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlation_id"`
	Retryable     bool   `json:"retryable"`
}

type apiErrorSpec struct {
	status    int
	code      string
	message   string
	retryable bool
}

var apiErrorSpecs = map[string]apiErrorSpec{
	"invalid_request":        {http.StatusBadRequest, "invalid_request", "The request is invalid.", false},
	"invalid_identifier":     {http.StatusBadRequest, "invalid_identifier", "The issue identifier is invalid.", false},
	"invalid_body":           {http.StatusBadRequest, "invalid_request", "The request body is invalid.", false},
	"unauthorized":           {http.StatusUnauthorized, "unauthorized", "Authentication is required.", false},
	"forbidden":              {http.StatusForbidden, "forbidden", "The request was not allowed.", false},
	"issue_not_found":        {http.StatusNotFound, "issue_not_found", "The requested issue was not found.", false},
	"not_found":              {http.StatusNotFound, "not_found", "The requested API route was not found.", false},
	"method_not_allowed":     {http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this route.", false},
	"refresh_unavailable":    {http.StatusConflict, "refresh_unavailable", "Refresh is unavailable in this mode.", false},
	"unsupported_media_type": {http.StatusUnsupportedMediaType, "unsupported_media_type", "Use JSON or form data for this request.", false},
	"runtime_unavailable":    {http.StatusServiceUnavailable, "runtime_unavailable", "Runtime state is temporarily unavailable.", true},
	"refresh_failed":         {http.StatusServiceUnavailable, "refresh_failed", "The refresh could not be completed.", false},
	"internal_error":         {http.StatusInternalServerError, "internal_error", "The request could not be completed.", false},
}

func (handler *PageHandler) nextCorrelationID() string {
	counter := handler.errorCounter.Add(1)
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)
	digest := hmac.New(sha256.New, handler.errorSeed[:])
	_, _ = digest.Write(message[:])
	return hex.EncodeToString(digest.Sum(nil)[:16])
}

func (handler *PageHandler) writeAPIError(w http.ResponseWriter, key string) {
	spec, ok := apiErrorSpecs[key]
	if !ok {
		spec = apiErrorSpecs["internal_error"]
	}
	handler.writeAPIErrorSpec(w, spec)
}

func (handler *PageHandler) writeAPIErrorSpec(w http.ResponseWriter, spec apiErrorSpec) {
	correlationID := handler.nextCorrelationID()
	w.Header().Set("X-Correlation-ID", correlationID)
	response := apiErrorResponse{Error: apiErrorBody{
		Code: spec.code, Message: spec.message, CorrelationID: correlationID, Retryable: spec.retryable,
	}}
	if err := writeJSON(w, spec.status, response); err != nil {
		setSecurityHeaders(w.Header())
	}
}

func apiErrorKeyForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	default:
		return "internal_error"
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	setSecurityHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, err = w.Write(encoded)
	return err
}

func cleanDisplay(value string, maximum int) (string, bool) {
	value = strings.ToValidUTF8(value, "�")
	var cleaned strings.Builder
	cleaned.Grow(min(len(value), maximum))
	spacePending := false
	for _, current := range value {
		if unicode.IsControl(current) {
			if current == '\n' || current == '\r' || current == '\t' {
				spacePending = cleaned.Len() > 0
			}
			continue
		}
		if spacePending && !unicode.IsSpace(current) {
			cleaned.WriteByte(' ')
		}
		spacePending = false
		cleaned.WriteRune(current)
	}
	result := strings.TrimSpace(cleaned.String())
	if maximum <= 0 || len(result) <= maximum {
		return result, false
	}
	const suffix = "…"
	cut := maximum - len(suffix)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(result[cut]) {
		cut--
	}
	return result[:cut] + suffix, true
}

func cleanMachine(value string, maximum int) string {
	value, _ = cleanDisplay(value, 0)
	if maximum <= 0 || len(value) <= maximum {
		return value
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func validIssueIdentifier(value string) bool {
	if value == "" || len(value) > maximumIdentifierBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return true
}

func validatedTrackerURL(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned, _ := cleanDisplay(*value, 2048)
	parsed, err := url.Parse(cleaned)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.TrimSpace(parsed.Hostname()) == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil
	}
	accepted := parsed.String()
	return &accepted
}

func eventCursorString(cursor domain.EventCursor) string {
	return fmt.Sprintf("%s:%d", cleanMachine(cursor.Epoch, 128), cursor.Sequence)
}

type emptyResponse struct{}

type stateResponse struct {
	GeneratedAt       time.Time                 `json:"generated_at"`
	Counts            stateCountsResponse       `json:"counts"`
	Running           []runningResponse         `json:"running"`
	Retrying          []retryResponse           `json:"retrying"`
	CodexTotals       tokenTotalsResponse       `json:"codex_totals"`
	RateLimits        *emptyResponse            `json:"rate_limits"`
	EventCursor       string                    `json:"event_cursor"`
	Candidates        []candidateResponse       `json:"candidates"`
	RecentEvents      []eventSummaryResponse    `json:"recent_events"`
	RecentEventsReset bool                      `json:"recent_events_reset"`
	Scheduler         schedulerResponse         `json:"scheduler"`
	Config            configStatusResponse      `json:"config"`
	Tracker           trackerStatusResponse     `json:"tracker"`
	Requests          []operatorRequestResponse `json:"requests"`
}

type stateCountsResponse struct {
	Running        int `json:"running"`
	Retrying       int `json:"retrying"`
	Candidates     int `json:"candidates"`
	Routable       int `json:"routable"`
	NeedsAttention int `json:"needs_attention"`
	Requests       int `json:"requests"`
	Errors         int `json:"errors"`
}

type tokenTotalsResponse struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

type runningTokenResponse struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type runningResponse struct {
	IssueID            string               `json:"issue_id"`
	IssueIdentifier    string               `json:"issue_identifier"`
	IssueURL           *string              `json:"issue_url"`
	State              string               `json:"state"`
	TurnCount          int                  `json:"turn_count"`
	LastEvent          string               `json:"last_event"`
	LastMessage        string               `json:"last_message"`
	StartedAt          time.Time            `json:"started_at"`
	LastEventAt        time.Time            `json:"last_event_at"`
	Tokens             runningTokenResponse `json:"tokens"`
	StartedDateTime    string               `json:"-"`
	StartedDisplayTime string               `json:"-"`
}

type retryResponse struct {
	IssueID         string    `json:"issue_id"`
	IssueIdentifier string    `json:"issue_identifier"`
	IssueURL        *string   `json:"issue_url"`
	Attempt         int       `json:"attempt"`
	DueAt           time.Time `json:"due_at"`
	Error           string    `json:"error"`
	DueDateTime     string    `json:"-"`
	DueDisplayTime  string    `json:"-"`
}

type schedulerResponse struct {
	Available bool   `json:"available"`
	Enabled   bool   `json:"enabled"`
	State     string `json:"state"`
	Message   string `json:"message"`
}

type configStatusResponse struct {
	State         string    `json:"state"`
	Digest        string    `json:"digest"`
	ActiveDigest  string    `json:"active_digest"`
	UsingLastGood bool      `json:"using_last_good"`
	ErrorCode     string    `json:"error_code"`
	Message       string    `json:"message"`
	ChangedAt     time.Time `json:"changed_at"`
}

type trackerStatusResponse struct {
	Kind                   string     `json:"kind"`
	Scope                  string     `json:"scope"`
	State                  string     `json:"state"`
	Stale                  bool       `json:"stale"`
	Retryable              bool       `json:"retryable"`
	LastAttemptAt          *time.Time `json:"last_attempt_at"`
	LastSuccessAt          *time.Time `json:"last_success_at"`
	RetryAt                *time.Time `json:"retry_at"`
	ErrorCode              string     `json:"error_code"`
	Message                string     `json:"message"`
	LastAttemptDateTime    string     `json:"-"`
	LastAttemptDisplayTime string     `json:"-"`
	LastSuccessDateTime    string     `json:"-"`
	LastSuccessDisplayTime string     `json:"-"`
	RetryDateTime          string     `json:"-"`
	RetryDisplayTime       string     `json:"-"`
}

type candidateResponse struct {
	IssueID        string                  `json:"issue_id"`
	Identifier     string                  `json:"identifier"`
	Title          string                  `json:"title"`
	State          string                  `json:"state"`
	Priority       *int                    `json:"priority"`
	URL            *string                 `json:"url"`
	Labels         []string                `json:"labels"`
	CreatedAt      *time.Time              `json:"created_at"`
	UpdatedAt      *time.Time              `json:"updated_at"`
	Routable       bool                    `json:"routable"`
	RoutingReasons []routingReasonResponse `json:"routing_reasons"`
	NeedsAttention bool                    `json:"needs_attention"`
	Stale          bool                    `json:"stale"`
	DetailURL      string                  `json:"-"`
}

type routingReasonResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type eventSummaryResponse struct {
	EventCursor string    `json:"event_cursor"`
	Type        string    `json:"type"`
	At          time.Time `json:"at"`
	Summary     string    `json:"summary"`
	Code        string    `json:"code"`
	DateTime    string    `json:"-"`
	DisplayTime string    `json:"-"`
}

type operatorChoiceResponse struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type operatorQuestionResponse struct {
	ID             string                   `json:"id"`
	Label          string                   `json:"label"`
	Description    string                   `json:"description"`
	Required       bool                     `json:"required"`
	AllowsMultiple bool                     `json:"allows_multiple"`
	Choices        []operatorChoiceResponse `json:"choices"`
}

type operatorRequestResponse struct {
	RequestID           string                     `json:"request_id"`
	IssueIdentifier     string                     `json:"issue_identifier"`
	Kind                string                     `json:"kind"`
	Title               string                     `json:"title"`
	Summary             string                     `json:"summary"`
	OpenedAt            time.Time                  `json:"opened_at"`
	WarningAt           time.Time                  `json:"warning_at"`
	DeadlineAt          time.Time                  `json:"deadline_at"`
	ExtensionsUsed      int                        `json:"extensions_used"`
	ExtensionsRemaining int                        `json:"extensions_remaining"`
	Choices             []operatorChoiceResponse   `json:"choices"`
	Questions           []operatorQuestionResponse `json:"questions"`
}

type issueResponse struct {
	IssueIdentifier string                   `json:"issue_identifier"`
	IssueID         string                   `json:"issue_id"`
	Status          string                   `json:"status"`
	Workspace       *emptyResponse           `json:"workspace"`
	Attempts        issueAttemptsResponse    `json:"attempts"`
	Running         *emptyResponse           `json:"running"`
	Retry           *emptyResponse           `json:"retry"`
	Logs            issueLogsResponse        `json:"logs"`
	RecentEvents    []eventSummaryResponse   `json:"recent_events"`
	LastError       *emptyResponse           `json:"last_error"`
	Tracked         emptyResponse            `json:"tracked"`
	Issue           issueSummaryResponse     `json:"issue"`
	Eligibility     issueEligibilityResponse `json:"eligibility"`
}

type issueAttemptsResponse struct {
	RestartCount        int `json:"restart_count"`
	CurrentRetryAttempt int `json:"current_retry_attempt"`
}

type issueLogsResponse struct {
	CodexSessionLogs []emptyResponse `json:"codex_session_logs"`
}

type issueSummaryResponse struct {
	Identifier           string                 `json:"identifier"`
	Title                string                 `json:"title"`
	Description          *string                `json:"description"`
	DescriptionTruncated bool                   `json:"description_truncated"`
	Priority             *int                   `json:"priority"`
	State                string                 `json:"state"`
	URL                  *string                `json:"url"`
	Labels               []string               `json:"labels"`
	Blockers             []issueBlockerResponse `json:"blockers"`
	CreatedAt            *time.Time             `json:"created_at"`
	UpdatedAt            *time.Time             `json:"updated_at"`
	CreatedDateTime      string                 `json:"-"`
	CreatedDisplayTime   string                 `json:"-"`
	UpdatedDateTime      string                 `json:"-"`
	UpdatedDisplayTime   string                 `json:"-"`
}

type issueBlockerResponse struct {
	Identifier *string `json:"identifier"`
	State      *string `json:"state"`
}

type issueEligibilityResponse struct {
	Routable bool                    `json:"routable"`
	Reasons  []routingReasonResponse `json:"reasons"`
}

type refreshReceiptResponse struct {
	Queued      bool      `json:"queued"`
	Coalesced   bool      `json:"coalesced"`
	RequestedAt time.Time `json:"requested_at"`
	Operations  []string  `json:"operations"`
}

func cleanCode(value string) string {
	if value == "" || len(value) > 64 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return ""
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return ""
		}
	}
	switch value {
	case "tracker_config", "tracker_auth", "tracker_transport", "tracker_response", "tracker_payload", "tracker_pagination", "tracker_rate_limited", "tracker_error", "invalid_workflow", "refresh_failed":
		return value
	default:
		return ""
	}
}

func tokenTotalsResponseFrom(value domain.TokenTotals) tokenTotalsResponse {
	return tokenTotalsResponse{InputTokens: value.InputTokens, OutputTokens: value.OutputTokens, TotalTokens: value.TotalTokens, SecondsRunning: value.SecondsRunning}
}

func runningTokenResponseFrom(value domain.TokenTotals) runningTokenResponse {
	return runningTokenResponse{InputTokens: value.InputTokens, OutputTokens: value.OutputTokens, TotalTokens: value.TotalTokens}
}

func routingReasonResponses(reasons []string) []routingReasonResponse {
	result := make([]routingReasonResponse, 0, min(len(reasons), 3))
	seen := make(map[string]struct{}, 3)
	for _, reason := range reasons {
		translated := routingReasonResponse{Code: "routing_unavailable", Message: "Routing details are unavailable."}
		switch reason {
		case "provider_not_dispatchable":
			translated = routingReasonResponse{Code: reason, Message: "Tracker marked this issue unavailable for dispatch."}
		case "missing_required_label":
			translated = routingReasonResponse{Code: reason, Message: "A required label is missing."}
		}
		if _, duplicate := seen[translated.Code]; duplicate {
			continue
		}
		seen[translated.Code] = struct{}{}
		result = append(result, translated)
	}
	return result
}

func eventSummary(event domain.Event) eventSummaryResponse {
	summary := eventSummaryResponse{EventCursor: eventCursorString(domain.EventCursor{Epoch: event.Epoch, Sequence: event.Sequence}), Type: "activity", At: event.At, Summary: "Activity occurred."}
	summary.DateTime, summary.DisplayTime = semanticTimeStrings(event.At)
	switch event.Type {
	case "queue.refreshed":
		summary.Type = event.Type
		summary.Summary = "Tracker work refreshed."
	case "queue.failed":
		summary.Type = event.Type
		summary.Summary = "Tracker refresh failed."
		summary.Code = "refresh_failed"
	case "configuration.changed":
		summary.Type = event.Type
		summary.Summary = "Configuration changed."
	}
	return summary
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func semanticTimeStrings(value time.Time) (string, string) {
	return value.UTC().Format(time.RFC3339Nano), value.Local().Format("Jan 2, 2006 3:04:05 PM MST")
}

func populateTrackerTimeViews(response *trackerStatusResponse) {
	if response.LastAttemptAt != nil {
		response.LastAttemptDateTime, response.LastAttemptDisplayTime = semanticTimeStrings(*response.LastAttemptAt)
	}
	if response.LastSuccessAt != nil {
		response.LastSuccessDateTime, response.LastSuccessDisplayTime = semanticTimeStrings(*response.LastSuccessAt)
	}
	if response.RetryAt != nil {
		response.RetryDateTime, response.RetryDisplayTime = semanticTimeStrings(*response.RetryAt)
	}
}

func populateIssueTimeViews(response *issueSummaryResponse) {
	if response.CreatedAt != nil {
		response.CreatedDateTime, response.CreatedDisplayTime = semanticIssueTimeStrings(*response.CreatedAt)
	}
	if response.UpdatedAt != nil {
		response.UpdatedDateTime, response.UpdatedDisplayTime = semanticIssueTimeStrings(*response.UpdatedAt)
	}
}

func semanticIssueTimeStrings(value time.Time) (string, string) {
	dateTime, _ := semanticTimeStrings(value)
	return dateTime, value.Local().Format("Jan 2, 2006 3:04 PM MST")
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
