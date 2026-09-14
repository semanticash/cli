package health

import (
	"context"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/broker"
)

func TestUnresolvedMutationsReportPotentialActivityGlobally(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	if checks := checkUnresolvedMutations(); len(checks) != 0 {
		t.Fatalf("empty archive: %+v", checks)
	}
	err := broker.RetainUnresolvedMutations(context.Background(), []broker.RawEvent{{EventID: "event", ToolName: "Bash"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	checks := checkUnresolvedMutations()
	if len(checks) != 1 {
		t.Fatalf("checks=%+v", checks)
	}
	for _, phrase := range []string{"1 potential mutation", "all repositories", "without mutation paths"} {
		if !strings.Contains(checks[0].Message, phrase) {
			t.Fatalf("missing %q: %+v", phrase, checks[0])
		}
	}
}
