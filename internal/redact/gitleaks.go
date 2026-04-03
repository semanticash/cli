package redact

import (
	"fmt"
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"
)

var (
	detector *detect.Detector
	initOnce sync.Once
	initErr  error

	// newDetectorFn lets tests force init failures without changing callers.
	newDetectorFn = defaultNewDetector
)

func defaultNewDetector() (*detect.Detector, error) {
	return detect.NewDetectorDefaultConfig()
}

func ensureInit() error {
	initOnce.Do(func() {
		detector, initErr = newDetectorFn()
		if initErr != nil {
			initErr = fmt.Errorf("redact: init gitleaks detector: %w", initErr)
		}
	})
	return initErr
}

// scan returns all findings for the given content.
func scan(content string) ([]report.Finding, error) {
	if err := ensureInit(); err != nil {
		return nil, err
	}
	return detector.DetectString(content), nil
}
