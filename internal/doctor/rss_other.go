//go:build !darwin && !linux && !freebsd

package doctor

func currentRSSMB() int64 {
	return 0
}
