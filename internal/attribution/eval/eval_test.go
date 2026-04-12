package eval

import (
	"testing"
)

func TestCorpus(t *testing.T) {
	summary := RunCorpus(Corpus)

	t.Log("\n" + FormatSummary(summary))

	for _, r := range summary.Results {
		t.Run(r.Name, func(t *testing.T) {
			if r.Passed {
				return
			}
			for _, e := range r.Errors {
				t.Error(e)
			}
		})
	}

	if summary.Failed > 0 {
		t.Fatalf("%d/%d cases failed", summary.Failed, summary.Total)
	}
}
