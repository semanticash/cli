//go:build !darwin

package launcher

// newManager returns ErrUnsupportedOS on platforms without a
// launcher backend.
func newManager() (manager, error) {
	return nil, ErrUnsupportedOS
}
