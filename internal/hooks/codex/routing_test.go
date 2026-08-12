package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/broker"
)

// TestApplyPatchEventsRouteToRegisteredRepo covers line and touch evidence
// from add, update, and delete operations.
func TestApplyPatchEventsRouteToRegisteredRepo(t *testing.T) {
	repos := []broker.RegisteredRepo{{
		RepoID:        "repo-1",
		Path:          fixtureRepo,
		CanonicalPath: fixtureRepo,
		Active:        true,
	}}

	cases := []struct {
		name     string
		envelope string
	}{
		{
			name: "content-bearing Add",
			envelope: strings.Join([]string{
				"*** Begin Patch",
				"*** Add File: main.go",
				"+package main",
				"*** End Patch",
			}, "\n"),
		},
		{
			name: "content-bearing Update",
			envelope: strings.Join([]string{
				"*** Begin Patch",
				"*** Update File: main.go",
				"@@",
				"-old",
				"+new",
				"*** End Patch",
			}, "\n"),
		},
		{
			name: "deletion-only Update",
			envelope: strings.Join([]string{
				"*** Begin Patch",
				"*** Update File: probe.go",
				"@@",
				"-func ToBeRemoved() string {",
				"-\treturn \"delete me\"",
				"-}",
				" func ToKeep() string {",
				" \treturn \"keep this\"",
				" }",
				"*** End Patch",
			}, "\n"),
		},
		{
			name: "pure Delete",
			envelope: strings.Join([]string{
				"*** Begin Patch",
				"*** Delete File: legacy.go",
				"*** End Patch",
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := applyPatchEvent(tc.envelope, fixtureRepo)
			out, err := (&Provider{}).BuildHookEvents(context.Background(), ev, newMemBlobStore())
			if err != nil {
				t.Fatalf("BuildHookEvents: %v", err)
			}
			if len(out) == 0 {
				t.Fatalf("no events emitted")
			}

			matches := broker.RouteEvents(out, repos)
			if len(matches) != 1 {
				t.Fatalf("RouteEvents matched %d repos, want 1 (the registered repo at %s); FilePaths=%v",
					len(matches), fixtureRepo, out[0].FilePaths)
			}
			if matches[0].Repo.RepoID != "repo-1" {
				t.Errorf("matched repo = %q, want repo-1", matches[0].Repo.RepoID)
			}
			if len(matches[0].Events) != len(out) {
				t.Errorf("repo match has %d events, want %d (no event should be dropped at routing)",
					len(matches[0].Events), len(out))
			}
		})
	}
}
