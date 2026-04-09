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
