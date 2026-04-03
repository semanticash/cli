package redact

import (
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
)

// ForceInitError makes subsequent String and Bytes calls fail until the
// returned cleanup function restores the default detector factory.
func ForceInitError(forcedErr error) (cleanup func()) {
	origFn := newDetectorFn
	origDetector := detector
	origErr := initErr

	detector = nil
	initOnce = sync.Once{}
	initErr = nil
	newDetectorFn = func() (*detect.Detector, error) {
		return nil, forcedErr
	}

	return func() {
		newDetectorFn = origFn
		detector = origDetector
		initOnce = sync.Once{}
		initErr = origErr
	}
}
