package web

import (
	"net/url"
	"strings"

	"github.com/coryj627/symphony/go/internal/app"
)

// Page is the common view model supplied to every rendered Symphony page.
type Page struct {
	Title                string
	Route                string
	Heading              string
	Mode                 string
	Flash                string
	FlashKind            string
	Status               string
	CSRFToken            string
	FocusTarget          string
	Scenario             string
	IssueListURL         string
	LiveRoute            string
	EventCursorID        string
	StateURL             string
	EventsURL            string
	ErrorSummary         []PageError
	ErrorSummaryInDialog bool
	Content              any
}

// InternalURL preserves the validated build-tag-only browser scenario across
// internal journeys. Production pages always have an empty Scenario.
func (page Page) InternalURL(target string) string {
	return internalURL(target, page.Scenario)
}

func internalURL(target, scenario string) string {
	if target == "" || scenario == "" {
		return target
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.IsAbs() || strings.HasPrefix(target, "//") {
		return target
	}
	values := parsed.Query()
	values.Set("__e2e_scenario", scenario)
	parsed.RawQuery = values.Encode()
	return parsed.String()
}

type PageError struct {
	ControlID string
	Message   string
}

type overviewContent struct {
	TrackerScope   string
	Mode           string
	Scheduler      schedulerResponse
	CandidateCount int
	RoutableCount  int
	RunningCount   int
	RetryingCount  int
	RequestCount   int
	ErrorCount     int
	Tracker        trackerStatusResponse
	Config         configStatusResponse
	ConfigError    bool
	TrackerError   bool
	StartDisabled  bool
	StartReason    string
	StopDisabled   bool
	StopReason     string
}

type issuesContent struct {
	Filters issueFilters
	Rows    []candidateResponse
	States  []string
}

type issueContent struct {
	Identifier  string
	Issue       issueSummaryResponse
	Eligibility issueEligibilityResponse
	Running     *runningResponse
	Retry       *retryResponse
	Requests    []operatorRequestResponse
	Activity    []eventSummaryResponse
	Logs        []logRecordView
	LogDegraded bool
}

type activityContent struct {
	Events []eventSummaryResponse
	Reset  bool
}

type configurationContent struct {
	View               app.ConfigView
	Values             app.ConfigValues
	RawSource          string
	CurrentSource      string
	Credential         app.CredentialState
	Errors             map[string]string
	DeleteConfirmation bool
}

type logsContent struct {
	Filters   logFilters
	Records   []logRecordView
	Degraded  bool
	OlderURL  string
	NewestURL string
}

type errorContent struct {
	Instruction string
}
