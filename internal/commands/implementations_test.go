package commands

import (
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/service/implementations"
)

func TestFormatImplementationOption(t *testing.T) {
	item := implementations.ListItem{
		ImplementationID: "6972dae0-1234-5678-9abc-def012345678",
		Title:            "Add roadmap voting across pulse repos",
		State:            "active",
		CommitCount:      1,
		Repos: []implementations.RepoSummary{
			{DisplayName: "pulse-api", Role: "origin"},
			{DisplayName: "pulse-sdk", Role: "downstream"},
			{DisplayName: "pulse-web", Role: "downstream"},
		},
	}

	got := stripANSI(formatImplementationOption(item))
	for _, want := range []string{
		"6972dae0",
		"Add roadmap voting across pulse repos",
		"active",
		"1 commit",
		"Repositories: pulse-api, pulse-sdk, pulse-web",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("option label missing %q: %s", want, got)
		}
	}
	if !strings.Contains(got, "\n") {
		t.Fatalf("option label should span multiple lines: %q", got)
	}
}

func TestDisplayImplementationTitle_Empty(t *testing.T) {
	if got := displayImplementationTitle("", 10); got != "-" {
		t.Fatalf("empty title: got %q want %q", got, "-")
	}
}

func TestTruncateDisplay(t *testing.T) {
	if got := truncateDisplay("abcdefghijkl", 8); got != "abcde..." {
		t.Fatalf("truncate: got %q want %q", got, "abcde...")
	}
}

func TestImplementationPickerTitle(t *testing.T) {
	got := implementationPickerTitle()
	for _, want := range []string{
		"Select an implementation",
		"ID",
		"TITLE",
		"STATE",
		"COMMITS",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("picker title missing %q: %q", want, got)
		}
	}
}

func TestRenderImplementationPlain_ShowsCommitFocusedSections(t *testing.T) {
	detail := &implementations.ImplementationDetail{
		ImplementationID: "4d741c1f-1234-5678-9abc-def012345678",
		Title:            "Add roadmap voting",
		Summary:          "Adds roadmap voting scaffolding across the API and web surfaces with a shared implementation story.",
		State:            "active",
		LastActivityAt:   0,
		Repos: []implementations.RepoDetail{
			{DisplayName: "pulse-api", Role: "origin", FirstSeenAt: 0, SessionCount: 2},
			{DisplayName: "pulse-web", Role: "downstream", FirstSeenAt: 0, SessionCount: 2},
		},
		RepoAttribution: []implementations.RepoAttribution{
			{DisplayName: "pulse-api", AIPercentage: 78},
			{DisplayName: "pulse-web", AIPercentage: 91},
		},
		Sessions: []implementations.SessionDetail{
			{Provider: "claude_code", SourceProjectPath: "/tmp/pulse/pulse-api"},
			{}, {}, {},
		},
		Commits: []implementations.CommitDetail{
			{DisplayName: "pulse-api", CommitHash: "abc1234", Subject: "Add roadmap voting API stubs"},
		},
		Timeline: []implementations.TimelineEntry{
			{Timestamp: 900, RepoName: "pulse-api", Kind: "tool", FilePath: "internal/voting/handler.go", FileOp: "edited", Summary: "Edit"},
			{Timestamp: 1000, RepoName: "pulse-api", Kind: "commit", Summary: "commit abc1234"},
		},
		TotalTokensIn:     957,
		TotalTokensOut:    823,
		TotalTokensCached: 16000,
	}

	got := renderImplementationPlain(detail, false)
	for _, want := range []string{
		"Add roadmap voting",
		"Implementation: 4d741c1f",
		"Started in pulse-api (Claude)",
		"Story",
		"Adds roadmap voting scaffolding across the API and web surfaces with a",
		"Repos",
		"Commits",
		"pulse-api    abc1234  Add roadmap voting API stubs",
		"Stats",
		"Implementation sessions: 4",
		"Session details: 2 in pulse-api, 2 in pulse-web",
		"AI attribution: 78% pulse-api · 91% pulse-web",
		"Tokens: 957 in / 823 out (+16k cached)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plain detail missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\nDetails\n") {
		t.Fatalf("plain detail should not show Details by default:\n%s", got)
	}
	if strings.Contains(got, ".claude/settings.json") {
		t.Fatalf("plain detail should filter internal config noise:\n%s", got)
	}
}

func TestImplementationAIAttribution(t *testing.T) {
	detail := &implementations.ImplementationDetail{
		RepoAttribution: []implementations.RepoAttribution{
			{DisplayName: "pulse-api", AIPercentage: 77.6},
			{DisplayName: "pulse-sdk", AIPercentage: 91.2},
		},
	}

	got := implementationAIAttribution(detail)
	want := "78% pulse-api · 91% pulse-sdk"
	if got != want {
		t.Fatalf("ai attribution: got %q want %q", got, want)
	}
}

func TestBuildSummaryLines_WrapsSummary(t *testing.T) {
	detail := &implementations.ImplementationDetail{
		Summary: "Adds roadmap voting scaffolding across the API, SDK, and web UI so the implementation card can show a concise product story before the repo and commit breakdown.",
	}

	got := buildSummaryLines(detail)
	if len(got) < 2 {
		t.Fatalf("expected wrapped summary lines, got %v", got)
	}
	if strings.Contains(got[0], "\n") {
		t.Fatalf("summary lines should already be split: %q", got[0])
	}
}

func TestBuildImplementationJSON_ContainsCardSections(t *testing.T) {
	detail := &implementations.ImplementationDetail{
		ImplementationID: "4d741c1f-1234-5678-9abc-def012345678",
		Title:            "Add roadmap voting",
		Summary:          "Adds roadmap voting scaffolding across the API and web UI.",
		State:            "active",
		LastActivityAt:   0,
		Repos: []implementations.RepoDetail{
			{DisplayName: "pulse-api", Role: "origin", FirstSeenAt: 0, SessionCount: 2},
			{DisplayName: "pulse-web", Role: "downstream", FirstSeenAt: 0, SessionCount: 2},
		},
		Sessions: []implementations.SessionDetail{
			{Provider: "claude_code", SourceProjectPath: "/tmp/pulse/pulse-api"},
			{}, {}, {},
		},
		Commits: []implementations.CommitDetail{
			{DisplayName: "pulse-api", CommitHash: "abc1234", Subject: "Add roadmap voting API stubs"},
		},
		Timeline: []implementations.TimelineEntry{
			{Timestamp: 900, RepoName: "pulse-api", Kind: "tool", FilePath: "internal/voting/handler.go", FileOp: "edited", Summary: "Edit"},
			{Timestamp: 1000, RepoName: "pulse-api", Kind: "commit", Summary: "commit abc1234"},
		},
		TotalTokensIn:     957,
		TotalTokensOut:    823,
		TotalTokensCached: 16000,
	}

	got := buildImplementationJSON(detail, true)
	if got.Card.Title != "Add roadmap voting" {
		t.Fatalf("card title: got %q", got.Card.Title)
	}
	if got.Card.Context != "Started in pulse-api (Claude)" {
		t.Fatalf("card context: got %q", got.Card.Context)
	}
	if len(got.Card.Story) == 0 || !strings.Contains(got.Card.Story[0], "Adds roadmap voting scaffolding") {
		t.Fatalf("expected story summary in card json: %#v", got.Card.Story)
	}
	if len(got.Card.Commits) != 1 || !strings.Contains(got.Card.Commits[0], "Add roadmap voting API stubs") {
		t.Fatalf("expected commit lines in card json: %#v", got.Card.Commits)
	}
	if len(got.Card.Details) == 0 {
		t.Fatalf("expected verbose details in card json")
	}
	if len(got.Card.Stats) == 0 {
		t.Fatalf("expected stats in card json")
	}
}

func TestBuildCommitLines(t *testing.T) {
	detail := &implementations.ImplementationDetail{
		Commits: []implementations.CommitDetail{
			{DisplayName: "pulse-api", CommitHash: "abc1234def", Subject: "Add roadmap voting API stubs"},
			{DisplayName: "pulse-web", CommitHash: "def5678abc"},
		},
	}

	got := buildCommitLines(detail)
	if len(got) != 2 {
		t.Fatalf("build commit lines: got %d want 2", len(got))
	}
	if !strings.Contains(got[0], "pulse-api") || !strings.Contains(got[0], "abc1234") || !strings.Contains(got[0], "Add roadmap voting API stubs") {
		t.Fatalf("unexpected first commit line: %q", got[0])
	}
	if !strings.Contains(got[1], "(no subject)") {
		t.Fatalf("expected missing subject fallback, got %q", got[1])
	}
}

func TestImplementationContextLine_PrefersOriginRepoDisplayName(t *testing.T) {
	detail := &implementations.ImplementationDetail{
		Repos: []implementations.RepoDetail{
			{DisplayName: "pulse-api", Role: "origin"},
			{DisplayName: "pulse-sdk", Role: "downstream"},
			{DisplayName: "pulse-web", Role: "downstream"},
		},
		Sessions: []implementations.SessionDetail{
			{Provider: "claude_code", SourceProjectPath: "/tmp/work/api"},
		},
	}

	got := implementationContextLine(detail)
	want := "Started in pulse-api (Claude)"
	if got != want {
		t.Fatalf("implementation context line: got %q want %q", got, want)
	}
}

func TestBuildVerboseTimelineLines_UsesRawSummary(t *testing.T) {
	detail := &implementations.ImplementationDetail{
		Timeline: []implementations.TimelineEntry{
			{Timestamp: 1000, RepoName: "pulse-api", Kind: "tool", Summary: "Read Read(/tmp/pulse/pulse-api/.claude/settings.json)"},
			{Timestamp: 2000, RepoName: "pulse-api", Kind: "tool", FilePath: "internal/voting/handler.go", FileOp: "edited", Summary: "Edit"},
			{Timestamp: 3000, RepoName: "pulse-api", Kind: "commit", Summary: "commit abc1234"},
		},
	}

	got := buildVerboseTimelineLines(detail)
	if len(got) != 3 {
		t.Fatalf("verbose timeline lines: got %d want 3", len(got))
	}
	if !strings.Contains(got[2], "Commit abc1234") {
		t.Fatalf("expected commit line in verbose timeline: %v", got)
	}
}
