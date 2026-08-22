package hooks

import "time"

// SetToolWindowDeadlineForTest overrides the capture deadline and returns a
// function that restores it. Tests using this helper must not run concurrently.
func SetToolWindowDeadlineForTest(d time.Duration) func() {
	previous := toolWindowDeadline
	toolWindowDeadline = d
	return func() { toolWindowDeadline = previous }
}
