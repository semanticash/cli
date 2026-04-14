package platform

import (
	"os"
	"testing"
)

func TestTermSignals_ContainsInterrupt(t *testing.T) {
	signals := TermSignals()
	found := false
	for _, s := range signals {
		if s == os.Interrupt {
			found = true
		}
	}
	if !found {
		t.Error("os.Interrupt not in TermSignals()")
	}
}

func TestTermSignals_NonEmpty(t *testing.T) {
	if len(TermSignals()) == 0 {
		t.Error("TermSignals() returned empty slice")
	}
}
