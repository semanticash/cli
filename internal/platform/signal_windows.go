//go:build windows

package platform

import "os"

// TermSignals returns the signals to listen for graceful shutdown.
// Windows does not support SIGTERM; os.Interrupt maps to CTRL_C_EVENT.
func TermSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
