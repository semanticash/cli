// Package matchercorpus verifies the public matcher corpus against the CLI
// attribution scorer.
package matchercorpus

import (
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/attribution/scoring"
)

//go:embed matcher_corpus.json
var corpusJSON []byte

type matcherCorpus struct {
	Version int           `json:"version"`
	Tiers   []string      `json:"tiers"`
	Cases   []matcherCase `json:"cases"`
}

type matcherCase struct {
	Name     string   `json:"name"`
	Claims   []string `json:"claims"`
	Added    []string `json:"added"`
	Expected []string `json:"expected"`
}

// canonicalTiers lists the supported corpus tiers.
var canonicalTiers = []string{"none", "exact", "normalized"}

// tierName returns the corpus name for a scorer tier.
func tierName(t int) (name string, ok bool) {
	switch t {
	case scoring.AlignNone:
		return "none", true
	case scoring.AlignExact:
		return "exact", true
	case scoring.AlignNormalized:
		return "normalized", true
	default:
		return "", false
	}
}

func TestTierNameFailsClosed(t *testing.T) {
	for tier, want := range map[int]string{
		scoring.AlignNone:       "none",
		scoring.AlignExact:      "exact",
		scoring.AlignNormalized: "normalized",
	} {
		if got, ok := tierName(tier); !ok || got != want {
			t.Errorf("tierName(%d) = (%q, %v), want (%q, true)", tier, got, ok, want)
		}
	}
	if _, ok := tierName(999); ok {
		t.Error("tierName(999): unknown tier should not be accepted")
	}
}

func TestMatcherCorpus(t *testing.T) {
	var corpus matcherCorpus
	if err := json.Unmarshal(corpusJSON, &corpus); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if corpus.Version != 1 {
		t.Fatalf("corpus version = %d, want 1", corpus.Version)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("corpus has no cases")
	}

	// Validate expected values against the corpus's canonical vocabulary.
	declared := map[string]bool{}
	for _, tr := range corpus.Tiers {
		if declared[tr] {
			t.Fatalf("corpus declares duplicate tier %q", tr)
		}
		declared[tr] = true
	}
	if len(declared) != len(canonicalTiers) {
		t.Fatalf("corpus declares %d tiers %v, want %v", len(declared), corpus.Tiers, canonicalTiers)
	}
	for _, tr := range canonicalTiers {
		if !declared[tr] {
			t.Fatalf("corpus is missing tier %q from its declared vocabulary", tr)
		}
	}

	seen := map[string]bool{}
	for _, c := range corpus.Cases {
		if c.Name == "" {
			t.Fatal("case with empty name")
		}
		if seen[c.Name] {
			t.Fatalf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true

		t.Run(c.Name, func(t *testing.T) {
			if len(c.Expected) != len(c.Added) {
				t.Fatalf("expected has %d tiers but added has %d lines", len(c.Expected), len(c.Added))
			}
			for _, want := range c.Expected {
				if !declared[want] {
					t.Fatalf("expected tier %q is not in the corpus's declared vocabulary", want)
				}
			}

			result, ok := scoring.AlignOrdered(scoring.NewClaimLines(c.Claims), c.Added)
			if !ok {
				t.Fatalf("alignment refused (over budget)")
			}
			if len(result) != len(c.Added) {
				t.Fatalf("result has %d lines, want %d", len(result), len(c.Added))
			}
			for i := range c.Added {
				got, ok := tierName(result[i].Tier)
				if !ok {
					t.Fatalf("added[%d]=%q: scorer emitted unknown tier %d; the corpus vocabulary is out of date", i, c.Added[i], result[i].Tier)
				}
				if got != c.Expected[i] {
					t.Errorf("added[%d]=%q: tier %q, want %q", i, c.Added[i], got, c.Expected[i])
				}
			}
		})
	}
}
