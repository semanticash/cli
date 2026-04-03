//go:build darwin || linux || freebsd

package doctor

import (
	"runtime"

	"golang.org/x/sys/unix"
)

func currentRSSMB() int64 {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0
	}

	bytes := int64(usage.Maxrss)
	if runtime.GOOS != "darwin" {
		bytes *= 1024
	}
	if bytes <= 0 {
		return 0
	}
	return bytes / (1024 * 1024)
}
