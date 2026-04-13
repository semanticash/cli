//go:build unix

package platform

import (
	"os"
	"syscall"
)

// TermSignals returns the signals to listen for graceful shutdown.
func TermSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
