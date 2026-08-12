package domain

import "time"

type AgentEvent struct {
	Type       string         `json:"type"`
	At         time.Time      `json:"at"`
	SessionID  string         `json:"session_id,omitempty"`
	ThreadID   string         `json:"thread_id,omitempty"`
	TurnID     string         `json:"turn_id,omitempty"`
	TurnCount  int            `json:"turn_count,omitempty"`
	Message    string         `json:"message,omitempty"`
	Tokens     TokenTotals    `json:"tokens"`
	RateLimits map[string]any `json:"rate_limits,omitempty"`
}
