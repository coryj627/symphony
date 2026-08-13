package codex

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTelemetryReplacesAbsoluteUsageAndRateLimitsIdempotently(t *testing.T) {
	events := make(chan SessionEvent, 16)
	session, transport := startTestSession(t, func(event SessionEvent) { events <- event })
	usage := map[string]any{
		"method": "thread/tokenUsage/updated",
		"params": map[string]any{
			"threadId": "thread-1", "turnId": "turn-1",
			"tokenUsage": map[string]any{
				"last":  tokenBreakdown(2, 3, 5),
				"total": tokenBreakdown(10, 20, 30),
			},
		},
	}
	transport.sendJSON(t, usage)
	transport.sendJSON(t, usage)
	transport.sendJSON(t, map[string]any{
		"method": "account/rateLimits/updated",
		"params": map[string]any{"rateLimits": map[string]any{
			"limitId": "primary", "primary": map[string]any{"usedPercent": 25, "windowDurationMins": 300},
		}},
	})
	transport.sendJSON(t, map[string]any{
		"method": "account/rateLimits/updated",
		"params": map[string]any{"rateLimits": map[string]any{
			"limitId": "secondary", "secondary": map[string]any{
				"usedPercent": 5, "windowDurationMins": 60, "resetsAt": int64(9007199254740993),
			},
		}},
	})
	awaitSessionEvent(t, events, SessionEventTelemetryUpdated)
	awaitSessionEvent(t, events, SessionEventTelemetryUpdated)
	awaitSessionEvent(t, events, SessionEventTelemetryUpdated)
	awaitSessionEvent(t, events, SessionEventTelemetryUpdated)

	snapshot := session.Telemetry()
	if snapshot.Tokens.InputTokens != 10 || snapshot.Tokens.OutputTokens != 20 || snapshot.Tokens.TotalTokens != 30 {
		t.Fatalf("%+v", snapshot.Tokens)
	}
	if snapshot.RateLimits["limitId"] != "secondary" || snapshot.RateLimits["primary"] != nil {
		t.Fatalf("%+v", snapshot.RateLimits)
	}
	snapshot.RateLimits["limitId"] = "mutated"
	if session.Telemetry().RateLimits["limitId"] != "secondary" {
		t.Fatal("telemetry snapshot exposed mutable session state")
	}
	secondary := session.Telemetry().RateLimits["secondary"].(map[string]any)
	if resetsAt, ok := secondary["resetsAt"].(json.Number); !ok || resetsAt.String() != "9007199254740993" {
		t.Fatalf("rate-limit integer lost precision or type: %T %v", secondary["resetsAt"], secondary["resetsAt"])
	}
}

func TestTelemetryIgnoresDeltaOnlyAndMismatchedUsage(t *testing.T) {
	events := make(chan SessionEvent, 16)
	session, transport := startTestSession(t, func(event SessionEvent) { events <- event })
	transport.sendJSON(t, map[string]any{
		"method": "thread/tokenUsage/updated",
		"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "tokenUsage": map[string]any{"delta": 99}},
	})
	awaitSessionEvent(t, events, SessionEventTelemetryIgnored)
	time.Sleep(time.Millisecond)
	if got := session.Telemetry().Tokens.TotalTokens; got != 0 {
		t.Fatalf("delta-only usage changed totals: %d", got)
	}
}

func tokenBreakdown(input, output, total int64) map[string]any {
	return map[string]any{
		"inputTokens": input, "cachedInputTokens": 0, "outputTokens": output,
		"reasoningOutputTokens": 0, "totalTokens": total,
	}
}
