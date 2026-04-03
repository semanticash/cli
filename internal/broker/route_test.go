package broker

import (
	"testing"
)

func makeRepo(path string) RegisteredRepo {
	return RegisteredRepo{
		RepoID:        "id-" + path,
		Path:          path,
		CanonicalPath: path,
		Active:        true,
	}
}

func makeEvent(id string, filePaths []string) RawEvent {
	return RawEvent{
		EventID:   id,
		Provider:  "claude_code",
		FilePaths: filePaths,
	}
}

func TestRouteEvents_SingleRepo(t *testing.T) {
	repos := []RegisteredRepo{
		makeRepo("/workspace/cli"),
	}

	events := []RawEvent{
		makeEvent("e1", []string{"/workspace/cli/main.go"}),
		makeEvent("e2", []string{"/workspace/cli/internal/service/worker.go"}),
	}

	matches := RouteEvents(events, repos)

	if len(matches) != 1 {
		t.Fatalf("expected 1 repo match, got %d", len(matches))
	}
	if len(matches[0].Events) != 2 {
		t.Errorf("expected 2 events for cli, got %d", len(matches[0].Events))
	}
}

func TestRouteEvents_CrossRepo(t *testing.T) {
	// Session launched from cli edits files in api.
	repos := []RegisteredRepo{
		makeRepo("/workspace/cli"),
		makeRepo("/workspace/api"),
	}

	events := []RawEvent{
		makeEvent("e1", []string{"/workspace/cli/main.go"}),
		makeEvent("e2", []string{"/workspace/api/src/handler.go"}),
		makeEvent("e3", []string{"/workspace/cli/go.mod"}),
	}

	matches := RouteEvents(events, repos)

	if len(matches) != 2 {
		t.Fatalf("expected 2 repo matches, got %d", len(matches))
	}

	// Find each repo's events.
	cliEvents := 0
	apiEvents := 0
	for _, m := range matches {
		switch m.Repo.CanonicalPath {
		case "/workspace/cli":
			cliEvents = len(m.Events)
		case "/workspace/api":
			apiEvents = len(m.Events)
		}
	}

	if cliEvents != 2 {
		t.Errorf("expected 2 events for cli, got %d", cliEvents)
	}
	if apiEvents != 1 {
		t.Errorf("expected 1 event for api, got %d", apiEvents)
	}
}

func TestRouteEvents_MultiRepoSingleEvent(t *testing.T) {
	// One event touches files in both repos.
	repos := []RegisteredRepo{
		makeRepo("/workspace/cli"),
		makeRepo("/workspace/api"),
	}

	events := []RawEvent{
		makeEvent("e1", []string{
			"/workspace/cli/main.go",
			"/workspace/api/src/handler.go",
		}),
	}

	matches := RouteEvents(events, repos)

	if len(matches) != 2 {
		t.Fatalf("expected 2 repo matches (deepest-match routes each file to its repo), got %d", len(matches))
	}

	for _, m := range matches {
		if len(m.Events) != 1 {
			t.Errorf("repo %s: expected 1 event, got %d", m.Repo.CanonicalPath, len(m.Events))
		}
	}
}

func TestRouteEvents_NoFilePaths_Skipped(t *testing.T) {
	repos := []RegisteredRepo{
		makeRepo("/workspace/cli"),
	}

	events := []RawEvent{
		makeEvent("e1", nil),                     // no file paths
		makeEvent("e2", []string{}),              // empty file paths
		makeEvent("e3", []string{"relative.go"}), // relative path (not routable)
	}

	matches := RouteEvents(events, repos)

	if len(matches) != 0 {
		t.Errorf("expected 0 matches for events without absolute file paths, got %d", len(matches))
	}
}

func TestRouteEvents_InactiveReposIgnored(t *testing.T) {
	// Only pass active repos (per contract: caller passes ListActiveRepos output).
	repos := []RegisteredRepo{
		makeRepo("/workspace/cli"),
	}

	events := []RawEvent{
		makeEvent("e1", []string{"/workspace/api/src/handler.go"}),
	}

	matches := RouteEvents(events, repos)

	if len(matches) != 0 {
		t.Errorf("expected 0 matches (api not registered), got %d", len(matches))
	}
}

func TestRouteEvents_NestedRepos(t *testing.T) {
	// Repos where one is nested under another.
	// Deepest-match rule: cli/main.go routes only to /projects/cli (deepest),
	// readme.md routes only to /projects (only match).
	repos := []RegisteredRepo{
		makeRepo("/workspace"),
		makeRepo("/workspace/cli"),
	}

	events := []RawEvent{
		makeEvent("e1", []string{"/workspace/cli/main.go"}),
		makeEvent("e2", []string{"/workspace/readme.md"}),
	}

	matches := RouteEvents(events, repos)

	parentEvents := 0
	childEvents := 0
	for _, m := range matches {
		switch m.Repo.CanonicalPath {
		case "/workspace":
			parentEvents = len(m.Events)
		case "/workspace/cli":
			childEvents = len(m.Events)
		}
	}

	if parentEvents != 1 {
		t.Errorf("parent repo: expected 1 event (only readme.md), got %d", parentEvents)
	}
	if childEvents != 1 {
		t.Errorf("child repo: expected 1 event (only main.go), got %d", childEvents)
	}
}

func TestRouteEvents_NestedRepos_CrossRepoEvent(t *testing.T) {
	// One event touches files in both parent and child repos.
	// Each file path routes to its deepest match, so the event appears in both.
	repos := []RegisteredRepo{
		makeRepo("/workspace"),
		makeRepo("/workspace/cli"),
	}

	events := []RawEvent{
		makeEvent("e1", []string{
			"/workspace/readme.md",
			"/workspace/cli/main.go",
		}),
	}

	matches := RouteEvents(events, repos)

	if len(matches) != 2 {
		t.Fatalf("expected 2 repo matches, got %d", len(matches))
	}
	for _, m := range matches {
		if len(m.Events) != 1 {
			t.Errorf("repo %s: expected 1 event, got %d", m.Repo.CanonicalPath, len(m.Events))
		}
	}
}

func TestRouteEvents_NestedRepos_MixedPaths(t *testing.T) {
	// One event touches a file in the parent (dad/) and another in the child
	// (dad/jane/). Deepest-match routes each path independently:
	//   dad/config.yaml  -> dad/   (only match)
	//   dad/jane/main.go -> dad/jane/ (deepest)
	// The event should appear exactly once in each repo.
	repos := []RegisteredRepo{
		makeRepo("/workspace/dad"),
		makeRepo("/workspace/dad/jane"),
	}

	events := []RawEvent{
		makeEvent("e1", []string{
			"/workspace/dad/config.yaml",
			"/workspace/dad/jane/main.go",
		}),
	}

	matches := RouteEvents(events, repos)

	if len(matches) != 2 {
		t.Fatalf("expected 2 repo matches, got %d", len(matches))
	}

	for _, m := range matches {
		if len(m.Events) != 1 {
			t.Errorf("repo %s: expected exactly 1 event, got %d", m.Repo.CanonicalPath, len(m.Events))
		}
	}

	// Verify both repos are present.
	seen := make(map[string]bool)
	for _, m := range matches {
		seen[m.Repo.CanonicalPath] = true
	}
	if !seen["/workspace/dad"] {
		t.Error("parent repo /workspace/dad missing from matches")
	}
	if !seen["/workspace/dad/jane"] {
		t.Error("child repo /workspace/dad/jane missing from matches")
	}
}

func TestRouteEvents_NestedRepos_DedupesPerEvent(t *testing.T) {
	// Two file paths in the same event both resolve to the same deepest repo.
	// The event should appear only once for that repo.
	repos := []RegisteredRepo{
		makeRepo("/workspace"),
		makeRepo("/workspace/cli"),
	}

	events := []RawEvent{
		makeEvent("e1", []string{
			"/workspace/cli/main.go",
			"/workspace/cli/internal/service.go",
		}),
	}

	matches := RouteEvents(events, repos)

	if len(matches) != 1 {
		t.Fatalf("expected 1 repo match, got %d", len(matches))
	}
	if matches[0].Repo.CanonicalPath != "/workspace/cli" {
		t.Errorf("expected deepest repo /workspace/cli, got %s", matches[0].Repo.CanonicalPath)
	}
	if len(matches[0].Events) != 1 {
		t.Errorf("expected 1 event (deduped), got %d", len(matches[0].Events))
	}
}

func TestRouteEvents_Empty(t *testing.T) {
	if matches := RouteEvents(nil, nil); matches != nil {
		t.Errorf("expected nil for empty input")
	}
	if matches := RouteEvents([]RawEvent{}, nil); matches != nil {
		t.Errorf("expected nil for empty events")
	}
}

func TestRouteNoPathEvents_MatchesSourceRepo(t *testing.T) {
	repos := []RegisteredRepo{
		makeRepo("/workspace/cli"),
		makeRepo("/workspace/api"),
	}

	events := []RawEvent{
		makeEvent("e1", nil),
	}

	match := RouteNoPathEvents(events, repos, "/workspace/cli")

	if match == nil {
		t.Fatal("expected match for source repo")
	}
	if match.Repo.CanonicalPath != "/workspace/cli" {
		t.Errorf("expected cli repo, got %s", match.Repo.CanonicalPath)
	}
}

func TestRouteNoPathEvents_NoMatch(t *testing.T) {
	repos := []RegisteredRepo{
		makeRepo("/workspace/cli"),
	}

	events := []RawEvent{
		makeEvent("e1", nil),
	}

	match := RouteNoPathEvents(events, repos, "/workspace/untracked")

	if match != nil {
		t.Errorf("expected nil for unregistered source path")
	}
}

func TestRouteNoPathEvents_SubdirMatchesParent(t *testing.T) {
	repos := []RegisteredRepo{
		makeRepo("/workspace/cli"),
		makeRepo("/workspace/api"),
	}

	events := []RawEvent{
		makeEvent("e1", nil),
	}

	// Session launched from /workspace/cli/cmd (subdirectory).
	match := RouteNoPathEvents(events, repos, "/workspace/cli/cmd")

	if match == nil {
		t.Fatal("expected subdirectory to match parent repo")
	}
	if match.Repo.CanonicalPath != "/workspace/cli" {
		t.Errorf("expected cli repo, got %s", match.Repo.CanonicalPath)
	}
	if len(match.Events) != 1 {
		t.Errorf("expected 1 event, got %d", len(match.Events))
	}
}

func TestRouteNoPathEvents_NestedRepos_DeepestWins(t *testing.T) {
	repos := []RegisteredRepo{
		makeRepo("/workspace"),
		makeRepo("/workspace/cli"),
	}

	events := []RawEvent{
		makeEvent("e1", nil),
	}

	// Source path is inside the nested repo - should match the deepest one.
	match := RouteNoPathEvents(events, repos, "/workspace/cli/cmd")

	if match == nil {
		t.Fatal("expected match for nested repo source path")
	}
	if match.Repo.CanonicalPath != "/workspace/cli" {
		t.Errorf("expected deepest repo /workspace/cli, got %s", match.Repo.CanonicalPath)
	}

	// Also test with repos in reverse order to ensure it's not order-dependent.
	reposReversed := []RegisteredRepo{
		makeRepo("/workspace/cli"),
		makeRepo("/workspace"),
	}
	match2 := RouteNoPathEvents(events, reposReversed, "/workspace/cli/cmd")
	if match2 == nil {
		t.Fatal("expected match (reversed order)")
	}
	if match2.Repo.CanonicalPath != "/workspace/cli" {
		t.Errorf("reversed order: expected deepest repo /workspace/cli, got %s", match2.Repo.CanonicalPath)
	}
}

func TestPathBelongsToRepo(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		repoRoot string
		want     bool
	}{
		{"exact match", "/workspace/cli", "/workspace/cli", true},
		{"subdirectory", "/workspace/cli/cmd", "/workspace/cli", true},
		{"deep subdirectory", "/workspace/cli/cmd/foo/bar", "/workspace/cli", true},
		{"different repo", "/workspace/api", "/workspace/cli", false},
		{"sibling with prefix", "/workspace/cli-tools", "/workspace/cli", false},
		{"parent of repo", "/workspace", "/workspace/cli", false},
		{"unrelated", "/tmp/scratch", "/workspace/cli", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PathBelongsToRepo(tt.path, tt.repoRoot)
			if got != tt.want {
				t.Errorf("PathBelongsToRepo(%q, %q) = %v, want %v", tt.path, tt.repoRoot, got, tt.want)
			}
		})
	}
}

func TestExtractFilePaths(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		expected []string
	}{
		{
			name:     "empty",
			json:     "",
			expected: nil,
		},
		{
			name:     "no_tools",
			json:     `{"content_types":["text"]}`,
			expected: nil,
		},
		{
			name:     "single_tool",
			json:     `{"tools":[{"name":"Edit","file_path":"/workspace/cli/main.go","file_op":"edit"}]}`,
			expected: []string{"/workspace/cli/main.go"},
		},
		{
			name:     "multiple_tools",
			json:     `{"tools":[{"name":"Edit","file_path":"/a/b.go"},{"name":"Read","file_path":"/c/d.go"}]}`,
			expected: []string{"/a/b.go", "/c/d.go"},
		},
		{
			name:     "deduplicates",
			json:     `{"tools":[{"name":"Read","file_path":"/a/b.go"},{"name":"Edit","file_path":"/a/b.go"}]}`,
			expected: []string{"/a/b.go"},
		},
		{
			name:     "skips_relative_paths",
			json:     `{"tools":[{"name":"Edit","file_path":"relative.go"}]}`,
			expected: nil,
		},
		{
			name:     "skips_empty_path",
			json:     `{"tools":[{"name":"Bash","file_path":""}]}`,
			expected: nil,
		},
		{
			name:     "cleans_paths",
			json:     `{"tools":[{"name":"Edit","file_path":"/a/b/../c/d.go"}]}`,
			expected: []string{"/a/c/d.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractFilePaths(tt.json)
			if len(got) != len(tt.expected) {
				t.Fatalf("ExtractFilePaths: got %v, want %v", got, tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("ExtractFilePaths[%d]: got %q, want %q", i, got[i], tt.expected[i])
				}
			}
		})
	}
}
