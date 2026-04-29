//go:build !darwin && !linux

package launcher

// newManager returns ErrUnsupportedOS on platforms without a
// launcher backend. Phase 3 will narrow this tag again to add
// Windows.
func newManager() (manager, error) {
	return nil, ErrUnsupportedOS
}
