package codex

import "fmt"

const continuationGuidance = `Continuation guidance:

- The previous Codex turn completed normally, but the tracker work item is still in an active state.
- This is continuation turn #%d of %d for the current agent run.
- Resume from the current workspace and workpad state instead of restarting from scratch.
- The original task instructions and prior turn context are already present in this thread, so do not restate them before acting.
- Focus on the remaining ticket work and do not end the turn while the issue stays active unless you are truly blocked.`

// ContinuationGuidance returns the fixed same-thread prompt for a later turn.
func ContinuationGuidance(turn, maximum int) (string, error) {
	if turn < 2 || maximum < 2 || turn > maximum {
		return "", fmt.Errorf("invalid continuation turn %d of %d", turn, maximum)
	}
	return fmt.Sprintf(continuationGuidance, turn, maximum), nil
}
