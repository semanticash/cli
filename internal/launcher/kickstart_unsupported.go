//go:build !darwin && !linux

package launcher

import "context"

// Kickstart returns ErrUnsupportedOS on platforms without a
// launcher backend. The target argument is accepted for source
// compatibility with the darwin signature but is never inspected.
func Kickstart(ctx context.Context, domainTarget string) error {
	_ = ctx
	_ = domainTarget
	return ErrUnsupportedOS
}
