package domain

import "time"

type RunStatus string

const (
	RunStatusPreparingWorkspace  RunStatus = "preparing_workspace"
	RunStatusBuildingPrompt      RunStatus = "building_prompt"
	RunStatusLaunchingAgent      RunStatus = "launching_agent_process"
	RunStatusInitializingSession RunStatus = "initializing_session"
	RunStatusStreamingTurn       RunStatus = "streaming_turn"
	RunStatusFinishing           RunStatus = "finishing"
	RunStatusSucceeded           RunStatus = "succeeded"
	RunStatusFailed              RunStatus = "failed"
	RunStatusTimedOut            RunStatus = "timed_out"
	RunStatusStalled             RunStatus = "stalled"
	RunStatusCanceled            RunStatus = "canceled_by_reconciliation"
)

type StopReason string

const (
	StopReasonNormal       StopReason = "normal"
	StopReasonFailed       StopReason = "failed"
	StopReasonTimedOut     StopReason = "timed_out"
	StopReasonStalled      StopReason = "stalled"
	StopReasonTerminal     StopReason = "terminal"
	StopReasonInactive     StopReason = "inactive"
	StopReasonUnroutable   StopReason = "unroutable"
	StopReasonMissing      StopReason = "missing"
	StopReasonOperatorStop StopReason = "operator_stop"
)

type RunResult struct {
	Reason       StopReason `json:"reason"`
	ErrorCode    string     `json:"error_code"`
	ErrorMessage string     `json:"error_message"`
	EndedAt      time.Time  `json:"ended_at"`
}
