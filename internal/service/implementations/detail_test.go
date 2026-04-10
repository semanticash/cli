package implementations

import (
	"database/sql"
	"testing"
)

func TestEnsureOriginRepo_MatchesSourceProjectPathSuffix(t *testing.T) {
	repos := []RepoDetail{
		{
			CanonicalPath: "/tmp/pulse/pulse-api",
			DisplayName:   "pulse-api",
			Role:          "downstream",
		},
		{
			CanonicalPath: "/tmp/pulse/pulse-sdk",
			DisplayName:   "pulse-sdk",
			Role:          "downstream",
		},
		{
			CanonicalPath: "/tmp/pulse/pulse-web",
			DisplayName:   "pulse-web",
			Role:          "downstream",
		},
	}
	sessions := []SessionDetail{
		{
			Provider:          "claude_code",
			ProviderSessionID: "sess-1",
			SourceProjectPath: "/tmp/work/api",
		},
	}

	ensureOriginRepo(repos, sessions)

	if repos[0].Role != "origin" {
		t.Fatalf("expected pulse-api to become origin, got %q", repos[0].Role)
	}
	if repos[1].Role != "downstream" {
		t.Fatalf("expected pulse-sdk to remain downstream, got %q", repos[1].Role)
	}
	if repos[2].Role != "downstream" {
		t.Fatalf("expected pulse-web to remain downstream, got %q", repos[2].Role)
	}
}

func TestImplementationSummaryFromMetadata(t *testing.T) {
	got := implementationSummaryFromMetadata(sql.NullString{
		String: `{"summary":"Adds roadmap voting across the API and web UI."}`,
		Valid:  true,
	})
	if got != "Adds roadmap voting across the API and web UI." {
		t.Fatalf("summary: got %q", got)
	}
}
