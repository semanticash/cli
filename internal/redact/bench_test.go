package redact

import (
	"strings"
	"sync"
	"testing"
)

func resetForBench(b *testing.B) {
	b.Helper()
	detector = nil
	initOnce = sync.Once{}
	initErr = nil
	newDetectorFn = defaultNewDetector
}

func BenchmarkString_SafePrompt_10KB(b *testing.B) {
	resetForBench(b)
	// Build a realistic 10KB safe prompt.
	line := "+\tfmt.Fprintf(out, \"AI attribution: %.1f%% (%d AI / %d human)\\n\", res.AIPercentage, res.AILines, res.HumanLines)\n"
	prompt := strings.Repeat(line, 10240/len(line))

	// Warm up the detector.
	_, _ = String(prompt)

	b.ResetTimer()
	b.SetBytes(int64(len(prompt)))
	for i := 0; i < b.N; i++ {
		_, _ = String(prompt)
	}
}

func BenchmarkString_MixedPrompt_10KB(b *testing.B) {
	resetForBench(b)
	safeLine := "+\tfmt.Fprintf(out, \"AI attribution: %.1f%% (%d AI / %d human)\\n\", res.AIPercentage, res.AILines, res.HumanLines)\n"
	webhook := "https://hooks.slack.com/" + "services/" + "T01234567/B01234567/xyzXYZ1234567890abcdefgh"
	prompt := strings.Repeat(safeLine, 9000/len(safeLine))
	prompt += "+\tslackURL := \"" + webhook + "\"\n"
	prompt += strings.Repeat(safeLine, 1000/len(safeLine))

	_, _ = String(prompt)

	b.ResetTimer()
	b.SetBytes(int64(len(prompt)))
	for i := 0; i < b.N; i++ {
		_, _ = String(prompt)
	}
}

func BenchmarkString_SafePrompt_100KB(b *testing.B) {
	resetForBench(b)
	line := "+\tresult.AIExactLines += fa.AIExactLines\n"
	prompt := strings.Repeat(line, 102400/len(line))

	_, _ = String(prompt)

	b.ResetTimer()
	b.SetBytes(int64(len(prompt)))
	for i := 0; i < b.N; i++ {
		_, _ = String(prompt)
	}
}
