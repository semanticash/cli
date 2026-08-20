package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/service"
)

func TestWriteTidyResultReportsToolWindowWork(t *testing.T) {
	res := &service.TidyResult{
		ToolWindowsRecovered: 33,
		ToolWindowsRemoved:   34,
		TombstonesRemoved:    2,
		Actions: []service.TidyAction{{
			Category: "toolwindow",
			ID:       "reclaim",
			Detail:   "reclaimed 1 degraded group, 34 members tombstoned",
		}},
	}

	var out bytes.Buffer
	writeTidyResult(&out, res)
	got := out.String()
	for _, want := range []string{
		"Tool windows: 33 recovered, 34 members reclaimed, 2 tombstones expired",
		"reclaim - reclaimed 1 degraded group, 34 members tombstoned",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Nothing to clean up") {
		t.Fatalf("tool-window work reported as empty:\n%s", got)
	}
}

func TestWriteTidyResultReportsErrorsWithoutChanges(t *testing.T) {
	var out bytes.Buffer
	writeTidyResult(&out, &service.TidyResult{Errors: 2})

	got := out.String()
	if !strings.Contains(got, "2 action(s) failed") {
		t.Fatalf("output does not report errors:\n%s", got)
	}
	if strings.Contains(got, "Nothing to clean up") {
		t.Fatalf("failed cleanup reported as empty:\n%s", got)
	}
}
