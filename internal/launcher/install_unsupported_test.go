//go:build !darwin && !linux && !windows

package launcher

import (
	"context"
	"errors"
	"testing"
)

// TestEnable_UnsupportedOS_BeforePathValidation pins the documented
// contract that Enable returns ErrUnsupportedOS on platforms without
// a launcher backend, regardless of whether the supplied binary
// path is valid. Callers using errors.Is(err, ErrUnsupportedOS) as
// their unsupported-host check rely on this ordering.
func TestEnable_UnsupportedOS_BeforePathValidation(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"empty path", ""},
		{"relative path", "./not-absolute"},
		{"missing absolute path", "/this/does/not/exist/anywhere"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Enable(context.Background(), c.path)
			if !errors.Is(err, ErrUnsupportedOS) {
				t.Errorf("Enable(%q) error = %v, want ErrUnsupportedOS", c.path, err)
			}
		})
	}
}

// TestDisable_UnsupportedOS pins that Disable also returns
// ErrUnsupportedOS on platforms without a launcher backend.
func TestDisable_UnsupportedOS(t *testing.T) {
	_, err := Disable(context.Background())
	if !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Disable error = %v, want ErrUnsupportedOS", err)
	}
}

// TestKickstart_UnsupportedOS pins that Kickstart returns
// ErrUnsupportedOS on platforms without a launcher backend, with
// any caller-supplied target.
func TestKickstart_UnsupportedOS(t *testing.T) {
	err := Kickstart(context.Background(), "any-target-value")
	if !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Kickstart error = %v, want ErrUnsupportedOS", err)
	}
}
