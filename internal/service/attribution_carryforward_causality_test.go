package service

import "testing"

// Same-file activity cannot unlock historical evidence for a modified file.
func TestCarryForward_ModifiedFileFailsClosedDespiteSameFileActivity(t *testing.T) {
	for _, tc := range []struct{ name, flag string }{{"v1", "0"}, {"v2", "1"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SEMANTICA_ATTRIBUTION_V2", tc.flag)
			w := newCFCommitWorld(t)

			commit1 := w.commitFile(t, "notes.txt", "# notes\n", "seed notes")
			// Historical evidence contains the line committed later.
			_ = insertEventWithPayload(t, w.h, w.bs, w.sessID, w.repoID, w.repoRoot,
				100_000, "notes.txt", "generated summary line\n")
			w.linkCheckpoint(t, commit1, 200_000, []string{"notes.txt"})

			// Current activity on the same file does not prove continuity.
			_ = insertEventWithPayload(t, w.h, w.bs, w.sessID, w.repoID, w.repoRoot,
				250_000, "notes.txt", "// work in progress\n")

			// The modified file adds the historical line.
			commit2 := w.commitFile(t, "notes.txt", "# notes\ngenerated summary line\n", "commit deferred line")
			w.linkCheckpoint(t, commit2, 300_000, []string{"notes.txt"})

			result := w.attribute(t, commit2)

			// Historical line attribution must fail closed.
			if result.AILines != 0 {
				t.Errorf("AILines=%d; modified-file carry-forward must fail closed even with same-file activity",
					result.AILines)
			}
		})
	}
}

// Unrelated provider activity cannot unlock historical evidence for a modified file.
func TestCarryForward_UnrelatedProviderDoesNotUnlockModifiedFile(t *testing.T) {
	for _, tc := range []struct{ name, flag string }{{"v1", "0"}, {"v2", "1"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SEMANTICA_ATTRIBUTION_V2", tc.flag)
			w := newCFCommitWorld(t)

			// Historical evidence contains a common heading.
			commit1 := w.commitFile(t, "CHANGELOG.md", "## [0.6.1]\n", "seed changelog")
			_ = insertEventWithPayload(t, w.h, w.bs, w.sessID, w.repoID, w.repoRoot,
				100_000, "CHANGELOG.md", "### Fixed\n")
			w.linkCheckpoint(t, commit1, 200_000, []string{"CHANGELOG.md"})

			// Current activity is limited to an unrelated file.
			_ = insertEventWithPayload(t, w.h, w.bs, w.sessID, w.repoID, w.repoRoot,
				250_000, "workflow.yml", "on: push\n")

			// The modified file adds the matching heading.
			commit2 := w.commitFile(t, "CHANGELOG.md", "## [0.6.1]\n### Fixed\n", "human heading")
			w.linkCheckpoint(t, commit2, 300_000, []string{"CHANGELOG.md"})

			result := w.attribute(t, commit2)

			f := fileByPath(t, result.Files, "CHANGELOG.md")
			if f.Classification == "ai" {
				t.Errorf("CHANGELOG.md classified ai; unrelated provider activity must not unlock carry-forward")
			}
			if result.AILines != 0 {
				t.Errorf("AILines = %d, want 0 (human changelog edit stays human)", result.AILines)
			}
			if len(f.Providers) != 0 {
				t.Errorf("providers = %v, want none for a human-only edit", f.Providers)
			}
		})
	}
}
