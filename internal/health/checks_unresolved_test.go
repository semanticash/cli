package health

import (
	"context"
	"os"
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
	if checks[0].Status != StatusOK || checks[0].Remediation != "" || assemble(checks).ExitCode() != 0 {
		t.Fatalf("normal retention made doctor unhealthy: %+v", checks)
	}
	for _, phrase := range []string{"1 potential mutation", "all repositories", "without mutation paths"} {
		if !strings.Contains(checks[0].Message, phrase) {
			t.Fatalf("missing %q: %+v", phrase, checks[0])
		}
	}
}

func TestUnresolvedMutationsUnreadableArchiveWarns(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	root, err := broker.UnresolvedMutationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := checkUnresolvedMutations()
	if len(checks) != 1 || checks[0].Status != StatusWarn || assemble(checks).ExitCode() != 1 {
		t.Fatalf("unreadable archive not reported: %+v", checks)
	}
}
