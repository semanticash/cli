//go:build !darwin && !linux && !windows

package launcher

// newManager returns ErrUnsupportedOS on platforms without a
// launcher backend (BSDs and any future targets without a per-user
// daemon manager equivalent to launchd, systemd user, or Task
// Scheduler).
func newManager() (manager, error) {
	return nil, ErrUnsupportedOS
}
