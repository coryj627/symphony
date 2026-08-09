package domain

import "time"

// EventCursor identifies one restart-scoped journal position.
type EventCursor struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence"`
}

// Event is one display-safe runtime state transition.
type Event struct {
	Epoch    string         `json:"epoch"`
	Sequence uint64         `json:"sequence"`
	Type     string         `json:"type"`
	At       time.Time      `json:"at"`
	Data     map[string]any `json:"data"`
}

// EventPage is a replay result. Reset asks a consumer to replace local state
// from a fresh snapshot rather than fabricating missing history.
type EventPage struct {
	Events       []Event     `json:"events"`
	LatestCursor EventCursor `json:"latest_cursor"`
	Reset        bool        `json:"reset"`
}
