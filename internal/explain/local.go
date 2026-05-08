package explain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
)

// localProvenance attempts to populate human_text from the repo's
// local lineage.db. It returns a non-empty string on a hit (the
// commit has a Semantica record with at least one linked session
// or attributed AI line) and an empty string on any of the
// documented local-provenance miss conditions:
//
//   - `.semantica/` is absent in the repo
//   - Semantica is disabled for the repo
//   - lineage.db is absent or unreadable
//   - the commit/checkpoint isn't linked in lineage.db
//   - the underlying explain service errors for any other reason
//
// Errors are intentionally swallowed: a miss here is not a command
// failure. Callers fall through to the next available fallback so the
// agent still gets a useful response.
func localProvenance(ctx context.Context, repoPath, commitHash string) string {
	semDir := filepath.Join(repoPath, ".semantica")
	if !util.IsEnabled(semDir) {
		return ""
	}
	if _, err := os.Stat(filepath.Join(semDir, "lineage.db")); err != nil {
		return ""
	}

	res, err := service.NewExplainService().Explain(ctx, service.ExplainInput{
		RepoPath: repoPath,
		Ref:      commitHash,
	})
	if err != nil {
		return ""
	}
		// No linked sessions and no AI lines means local provenance
		// has no useful record for this commit.
	if res.SessionCount == 0 && res.AILines == 0 {
		return ""
	}
	return formatProvenance(res)
}

// formatProvenance renders an ExplainResult as a multi-line string
// the agent prints verbatim under `mode: provenance`. The shape
// mirrors the user-facing `semantica explain` terminal output so
// the SKILL surface and terminal surface stay coherent. Pure
// function: no I/O, no service calls. Hand-built fixtures in
// tests pin the format.
func formatProvenance(res *service.ExplainResult) string {
	var b strings.Builder

	subject := res.CommitSubject
	if subject == "" {
		subject = "(no subject)"
	}
	fmt.Fprintf(&b, "Commit %s - %s\n", shortHash(res.CommitHash), subject)
	fmt.Fprintf(&b, "%d files changed (+%d/-%d)\n",
		res.FilesChanged, res.LinesAdded, res.LinesDeleted)
	b.WriteByte('\n')

	b.WriteString("AI involvement:\n")
	if res.SessionCount > 0 {
		fmt.Fprintf(&b, "  %d %s (%d root, %d %s)\n",
			res.SessionCount, plural(res.SessionCount, "session"),
			res.RootSessions,
			res.Subagents, plural(res.Subagents, "subagent"))
	} else {
		b.WriteString("  No agent sessions linked to this commit\n")
	}
	fmt.Fprintf(&b, "  %.1f%% AI-Attributed (%d AI / %d human)\n",
		res.AIPercentage, res.AILines, res.HumanLines)
	if res.FilesChanged > 0 {
		fmt.Fprintf(&b, "  %d of %d files contain AI-produced lines\n",
			res.FilesWithAI, res.FilesChanged)
	}

	if len(res.TopFiles) > 0 {
		b.WriteString("\nTop edited files:\n")
		for _, f := range res.TopFiles {
			fmt.Fprintf(&b, "  %s (+%d/-%d)\n", f.Path, f.Added, f.Deleted)
		}
	}

	if res.Summary != nil {
		writeSummary(&b, res.Summary)
	}

	return strings.TrimRight(b.String(), "\n")
}

// writeSummary appends the Playbook section that lives in
// `summary.json`. Sections are emitted only when populated so
// short summaries stay short.
func writeSummary(b *strings.Builder, s *service.NarrativeResultJSON) {
	b.WriteString("\n")
	if s.Title != "" {
		fmt.Fprintf(b, "[Playbook] %s\n", s.Title)
	} else {
		b.WriteString("[Playbook]\n")
	}
	if s.Intent != "" {
		fmt.Fprintf(b, "\nIntent:\n%s\n", s.Intent)
	}
	if s.Outcome != "" {
		fmt.Fprintf(b, "\nOutcome:\n%s\n", s.Outcome)
	}
	writeBulletList(b, "Learnings:", s.Learnings)
	writeBulletList(b, "Friction:", s.Friction)
	writeBulletList(b, "Open items:", s.OpenItems)
}

func writeBulletList(b *strings.Builder, header string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", header)
	for _, it := range items {
		fmt.Fprintf(b, "  - %s\n", it)
	}
}

// shortHash mirrors util.ShortID: an 8-char prefix when the input
// is long enough, otherwise the input as-is. Inlined here so the
// explain package stays self-contained for the formatter.
func shortHash(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// plural returns word with an "s" appended when n != 1. Used for
// "session"/"sessions" and "subagent"/"subagents" so the
// rendered prose reads naturally to an agent that may quote or
// summarize the output.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
