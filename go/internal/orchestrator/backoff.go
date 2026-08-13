package orchestrator

import "time"

const (
	ContinuationDelay = time.Second
	failureBaseDelay  = 10 * time.Second
)

func FailureDelay(attempt int, cap time.Duration) time.Duration {
	if cap <= 0 {
		return 0
	}
	if attempt < 1 {
		return cap
	}
	delay := failureBaseDelay
	if delay >= cap {
		return cap
	}
	for current := 1; current < attempt; current++ {
		if delay >= cap-delay {
			return cap
		}
		delay *= 2
	}
	return delay
}
