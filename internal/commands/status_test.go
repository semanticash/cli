package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/service"
)

func TestRenderStatusCard(t *testing.T) {
	res := &service.StatusResult{
		Enabled:            true,
		RepoRoot:           "/tmp/repo",
		Connected:          false,
		HasRemote:          true,
		Endpoint:           "https://example.com",
		RepoProvider:       "github",
		WorkspaceTierTitle: "Free",
		UpdateAvailable:    true,
		LatestVersion:      "v0.2.0",
		AutoPlaybook:       true,
		GitTrailers:        true,
		Providers:          []string{"claude-code"},
		LastCheckpoint: &service.LastCheckpointInfo{
			ID:        "83cff5a8-1234-5678-9abc-def012345678",
			CreatedAt: 1710000000000,
			Kind:      "baseline",
			Message:   "Baseline snapshot at enable time",
		},
	}

	authState := auth.AuthState{
		Authenticated: true,
		Email:         "dev@example.com",
		Source:        "session",
	}

	got := stripANSI(renderStatusCard(res, authState))

	for _, want := range []string{
		"Semantica",
		"Code, with provenance.",
		"Enabled in /tmp/repo",
		"Authenticated: yes (dev@example.com)",
		"Store: " + filepath.Join("/tmp/repo", ".semantica"),
		"Workspace tier: Free",
		"Connected: no",
		"Endpoint: https://example.com",
		"Settings",
		"Remote: github",
		"Auto-playbook: enabled",
		"Git Trailers: enabled",
		"Agents: claude-code",
		"Update available",
		"Version: v0.2.0",
		"Install:",
		cliUpgradeCommand,
		"Last checkpoint",
		"baseline",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status card missing %q:\n%s", want, got)
		}
	}
}

func TestRenderStatusPlain_ShowsPendingProvenance(t *testing.T) {
	res := &service.StatusResult{
		Enabled:      true,
		RepoRoot:     "/tmp/repo",
		Connected:    true,
		HasRemote:    true,
		Endpoint:     "https://example.com",
		RepoProvider: "github",
		PendingProvenance: &service.PendingProvenanceInfo{
			Count:                3,
			HasLastCommit:        true,
			SinceLastCommitCount: 3,
		},
	}

	got := renderStatusPlain(res, auth.AuthState{Authenticated: true})
	for _, want := range []string{
		"Sync",
		"Pending provenance: 3 turns since last commit",
		"Uploads on the next commit checkpoint, or when confirmed from `semantica connect`.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q:\n%s", want, got)
		}
	}
}
