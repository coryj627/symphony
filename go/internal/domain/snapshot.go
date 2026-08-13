package domain

import "time"

type RefreshReceipt struct {
	Queued      bool      `json:"queued"`
	Coalesced   bool      `json:"coalesced"`
	RequestedAt time.Time `json:"requested_at"`
	Operations  []string  `json:"operations"`
}

type SchedulerStatus struct {
	Available bool   `json:"available"`
	Enabled   bool   `json:"enabled"`
	State     string `json:"state"`
	Message   string `json:"message"`
}

type ConfigStatus struct {
	State         string    `json:"state"`
	Digest        string    `json:"digest"`
	ActiveDigest  string    `json:"active_digest"`
	UsingLastGood bool      `json:"using_last_good"`
	ErrorCode     string    `json:"error_code"`
	Message       string    `json:"message"`
	ChangedAt     time.Time `json:"changed_at"`
}

type TrackerStatus struct {
	Kind          string     `json:"kind"`
	Scope         string     `json:"scope"`
	State         string     `json:"state"`
	Stale         bool       `json:"stale"`
	Retryable     bool       `json:"retryable"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	RetryAt       *time.Time `json:"retry_at"`
	ErrorCode     string     `json:"error_code"`
	Message       string     `json:"message"`
}

type TokenTotals struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

type RunningRow struct {
	IssueID         string      `json:"issue_id"`
	IssueIdentifier string      `json:"issue_identifier"`
	IssueURL        *string     `json:"issue_url"`
	State           string      `json:"state"`
	SessionID       string      `json:"session_id"`
	TurnCount       int         `json:"turn_count"`
	LastEvent       string      `json:"last_event"`
	LastMessage     string      `json:"last_message"`
	StartedAt       time.Time   `json:"started_at"`
	LastEventAt     time.Time   `json:"last_event_at"`
	Tokens          TokenTotals `json:"tokens"`
}

type RetryRow struct {
	IssueID         string    `json:"issue_id"`
	IssueIdentifier string    `json:"issue_identifier"`
	IssueURL        *string   `json:"issue_url"`
	Attempt         int       `json:"attempt"`
	DueAt           time.Time `json:"due_at"`
	Error           string    `json:"error"`
}

type CandidateRow struct {
	Issue          Issue    `json:"issue"`
	Routable       bool     `json:"routable"`
	RoutingReasons []string `json:"routing_reasons"`
}

type IssueDetail struct {
	Issue          Issue       `json:"issue"`
	Status         string      `json:"status"`
	Routable       bool        `json:"routable"`
	RoutingReasons []string    `json:"routing_reasons"`
	Workspace      *Workspace  `json:"workspace"`
	Attempt        *int        `json:"attempt"`
	Running        *RunningRow `json:"running"`
	Retry          *RetryRow   `json:"retry"`
}

type Snapshot struct {
	GeneratedAt time.Time         `json:"generated_at"`
	EventCursor EventCursor       `json:"event_cursor"`
	Scheduler   SchedulerStatus   `json:"scheduler"`
	Candidates  []CandidateRow    `json:"candidates"`
	Running     []RunningRow      `json:"running"`
	Retrying    []RetryRow        `json:"retrying"`
	Requests    []OperatorRequest `json:"requests"`
	CodexTotals TokenTotals       `json:"codex_totals"`
	RateLimits  map[string]any    `json:"rate_limits"`
	Config      ConfigStatus      `json:"config"`
	Tracker     TrackerStatus     `json:"tracker"`
}

// EmptySnapshot returns the safe collection shape used before a runtime has
// provider state. RateLimits intentionally remains nil until Codex exists.
func EmptySnapshot() Snapshot {
	return Snapshot{
		Candidates: []CandidateRow{},
		Running:    []RunningRow{},
		Retrying:   []RetryRow{},
		Requests:   []OperatorRequest{},
	}
}

func (snapshot Snapshot) Clone() (Snapshot, error) {
	clone := snapshot
	clone.Candidates = make([]CandidateRow, len(snapshot.Candidates))
	for index, candidate := range snapshot.Candidates {
		issue, err := candidate.Issue.Clone()
		if err != nil {
			return Snapshot{}, err
		}
		issue = preserveIssueCollections(candidate.Issue, issue)
		clone.Candidates[index] = CandidateRow{
			Issue: issue, Routable: candidate.Routable,
			RoutingReasons: append([]string{}, candidate.RoutingReasons...),
		}
	}
	clone.Running = cloneRunningRows(snapshot.Running)
	clone.Retrying = cloneRetryRows(snapshot.Retrying)
	clone.Requests = make([]OperatorRequest, len(snapshot.Requests))
	for index, request := range snapshot.Requests {
		clone.Requests[index] = request.Clone()
	}
	if snapshot.RateLimits != nil {
		rateLimits, err := cloneNativeRef(snapshot.RateLimits)
		if err != nil {
			return Snapshot{}, err
		}
		clone.RateLimits = rateLimits
	}
	clone.Tracker.LastAttemptAt = cloneTime(snapshot.Tracker.LastAttemptAt)
	clone.Tracker.LastSuccessAt = cloneTime(snapshot.Tracker.LastSuccessAt)
	clone.Tracker.RetryAt = cloneTime(snapshot.Tracker.RetryAt)
	return clone, nil
}

func (detail IssueDetail) Clone() (IssueDetail, error) {
	clone := detail
	issue, err := detail.Issue.Clone()
	if err != nil {
		return IssueDetail{}, err
	}
	clone.Issue = preserveIssueCollections(detail.Issue, issue)
	clone.RoutingReasons = append([]string{}, detail.RoutingReasons...)
	if detail.Workspace != nil {
		workspace := *detail.Workspace
		clone.Workspace = &workspace
	}
	if detail.Attempt != nil {
		attempt := *detail.Attempt
		clone.Attempt = &attempt
	}
	if detail.Running != nil {
		running := cloneRunningRows([]RunningRow{*detail.Running})
		clone.Running = &running[0]
	}
	if detail.Retry != nil {
		retrying := cloneRetryRows([]RetryRow{*detail.Retry})
		clone.Retry = &retrying[0]
	}
	return clone, nil
}

func preserveIssueCollections(source Issue, clone Issue) Issue {
	if source.Labels != nil && clone.Labels == nil {
		clone.Labels = []string{}
	}
	if source.BlockedBy != nil && clone.BlockedBy == nil {
		clone.BlockedBy = []BlockerRef{}
	}
	return clone
}

func cloneRunningRows(rows []RunningRow) []RunningRow {
	if rows == nil {
		return nil
	}
	clone := append(make([]RunningRow, 0, len(rows)), rows...)
	for index := range clone {
		clone[index].IssueURL = cloneString(rows[index].IssueURL)
	}
	return clone
}

func cloneRetryRows(rows []RetryRow) []RetryRow {
	if rows == nil {
		return nil
	}
	clone := append(make([]RetryRow, 0, len(rows)), rows...)
	for index := range clone {
		clone[index].IssueURL = cloneString(rows[index].IssueURL)
	}
	return clone
}
