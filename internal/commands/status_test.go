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

// Disabled repos should render as plain "not enabled".
func TestRenderStatusPlain_DisabledIsNotStale(t *testing.T) {
	res := &service.StatusResult{
		Enabled:  false,
		RepoRoot: "/tmp/repo",
		// The service layer clears StaleReason for disabled repos.
	}
	got := renderStatusPlain(res, auth.AuthState{Authenticated: true})
	if strings.Contains(got, "stale local state") {
		t.Errorf("disabled repo must not mention stale local state:\n%s", got)
	}
	if !strings.Contains(got, "Semantica: not enabled") {
		t.Errorf("expected plain not-enabled header:\n%s", got)
	}
	if strings.Contains(got, "semantica tidy --apply") {
		t.Errorf("disabled repo must not point at tidy --apply:\n%s", got)
	}
}

// TestRenderStatusPlain_StaleReason confirms stale local state renders
// with a specific remediation.
func TestRenderStatusPlain_StaleReason(t *testing.T) {
	res := &service.StatusResult{
		Enabled:     false,
		RepoRoot:    "/tmp/repo",
		StaleReason: "no-repo-row",
	}
	got := renderStatusPlain(res, auth.AuthState{Authenticated: true})
	for _, want := range []string{
		"not enabled (stale local state: no local repository row)",
		"semantica tidy --apply",
		"semantica enable --force",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
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
