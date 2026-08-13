package codex

import (
	"bytes"
	"encoding/json"
	"errors"
)

// TelemetrySnapshot contains the latest absolute usage and rate-limit values.
type TelemetrySnapshot struct {
	Tokens     TokenUsageBreakdown
	RateLimits map[string]any
}

// Telemetry returns a detached snapshot; repeated absolute notifications replace values.
func (session *Session) Telemetry() TelemetrySnapshot {
	session.mu.Lock()
	defer session.mu.Unlock()
	return TelemetrySnapshot{
		Tokens:     session.telemetry.Tokens,
		RateLimits: cloneJSONMap(session.telemetry.RateLimits),
	}
}

func (session *Session) handleTokenUsage(raw json.RawMessage) {
	notification, ok := decodeAbsoluteTokenUsage(raw)
	if !ok {
		session.emit(SessionEvent{Type: SessionEventTelemetryIgnored, Summary: "An unsupported Codex token-usage notification was ignored."})
		return
	}
	session.mu.Lock()
	if session.threadID == "" || notification.ThreadID != session.threadID {
		session.mu.Unlock()
		session.emit(SessionEvent{Type: SessionEventTelemetryIgnored, Summary: "Token usage for a different Codex thread was ignored."})
		return
	}
	session.telemetry.Tokens = notification.TokenUsage.Total
	snapshot := session.telemetry.Tokens
	session.mu.Unlock()
	session.emit(SessionEvent{
		Type: SessionEventTelemetryUpdated, ThreadID: notification.ThreadID, TurnID: notification.TurnID,
		Tokens: snapshot, Summary: "Absolute Codex token usage was updated.",
	})
}

func decodeAbsoluteTokenUsage(raw json.RawMessage) (ThreadTokenUsageUpdatedNotification, bool) {
	root, err := decodeJSONObject(raw)
	if err != nil {
		return ThreadTokenUsageUpdatedNotification{}, false
	}
	var notification ThreadTokenUsageUpdatedNotification
	if json.Unmarshal(root["threadId"], &notification.ThreadID) != nil || notification.ThreadID == "" ||
		json.Unmarshal(root["turnId"], &notification.TurnID) != nil || notification.TurnID == "" {
		return ThreadTokenUsageUpdatedNotification{}, false
	}
	usage, err := decodeJSONObject(root["tokenUsage"])
	if err != nil {
		return ThreadTokenUsageUpdatedNotification{}, false
	}
	last, ok := decodeTokenBreakdown(usage["last"])
	if !ok {
		return ThreadTokenUsageUpdatedNotification{}, false
	}
	total, ok := decodeTokenBreakdown(usage["total"])
	if !ok {
		return ThreadTokenUsageUpdatedNotification{}, false
	}
	notification.TokenUsage.Last = last
	notification.TokenUsage.Total = total
	if rawWindow, exists := usage["modelContextWindow"]; exists && string(rawWindow) != "null" {
		var window int64
		if json.Unmarshal(rawWindow, &window) != nil {
			return ThreadTokenUsageUpdatedNotification{}, false
		}
		notification.TokenUsage.ModelContextWindow = &window
	}
	return notification, true
}

func decodeTokenBreakdown(raw json.RawMessage) (TokenUsageBreakdown, bool) {
	fields, err := decodeJSONObject(raw)
	if err != nil {
		return TokenUsageBreakdown{}, false
	}
	values := []*int64{
		new(int64), new(int64), new(int64), new(int64), new(int64),
	}
	names := []string{"cachedInputTokens", "inputTokens", "outputTokens", "reasoningOutputTokens", "totalTokens"}
	for index, name := range names {
		rawValue, exists := fields[name]
		if !exists || json.Unmarshal(rawValue, values[index]) != nil || *values[index] < 0 {
			return TokenUsageBreakdown{}, false
		}
	}
	return TokenUsageBreakdown{
		CachedInputTokens: *values[0], InputTokens: *values[1], OutputTokens: *values[2],
		ReasoningOutputTokens: *values[3], TotalTokens: *values[4],
	}, true
}

func (session *Session) handleRateLimits(raw json.RawMessage) {
	root, err := decodeJSONObject(raw)
	if err != nil {
		session.emit(SessionEvent{Type: SessionEventTelemetryIgnored, Summary: "An unsupported Codex rate-limit notification was ignored."})
		return
	}
	rateLimits, err := decodeJSONMap(root["rateLimits"])
	if err != nil {
		session.emit(SessionEvent{Type: SessionEventTelemetryIgnored, Summary: "An unsupported Codex rate-limit notification was ignored."})
		return
	}
	session.mu.Lock()
	session.telemetry.RateLimits = cloneJSONMap(rateLimits)
	snapshot := cloneJSONMap(session.telemetry.RateLimits)
	session.mu.Unlock()
	session.emit(SessionEvent{
		Type: SessionEventTelemetryUpdated, RateLimits: snapshot,
		Summary: "Codex rate-limit status was updated.",
	})
}

func cloneJSONMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return nil
	}
	clone, err := decodeJSONMap(encoded)
	if err != nil {
		return nil
	}
	return clone
}

func decodeJSONMap(source []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		if err == nil {
			err = errors.New("JSON value is not an object")
		}
		return nil, err
	}
	return value, nil
}
