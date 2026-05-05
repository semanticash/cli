// Package eval defines the attribution evaluation corpus and runner.
// It tests the domain packages (events, scoring, reporting) directly
// without any database, blob store, or service orchestration.
package eval

import (
	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/attribution/events"
	"github.com/semanticash/cli/internal/attribution/reporting"
)

// EvalCase is a single attribution evaluation fixture.
type EvalCase struct {
	Name        string
	Description string

	// Inputs
	Diff     string            // unified diff text
	Events   []events.EventRow // event rows with inline payloads
	RepoRoot string            // repo root for path normalization

	// Evidence context (optional)
	CarryForwardFiles    map[string]bool                  // files known to come from historical lookback
	TouchOriginOverrides map[string]reporting.TouchOrigin // explicit touch origin overrides for testing

	// Expected outcomes
	Expected ExpectedResult
}

// ExpectedResult defines the ground-truth attribution for a case.
type ExpectedResult struct {
	AIPercentage  float64 // headline AI% (within tolerance)
	Evidence      string  // commit-level: "High", "Medium", "Low"
	FallbackCount int     // expected number of fallback files

	Files []ExpectedFile // per-file expectations
}

// ExpectedFile defines the expected attribution for a single file.
type ExpectedFile struct {
	Path                 string
	AILines              int
	HumanLines           int
	PrimaryEvidence      reporting.EvidenceClass
	ContributingEvidence []reporting.EvidenceClass // all evidence classes that should appear
	Notes                string                    // optional: known ambiguity or edge case
}

// claudePayload builds an assistant payload blob in Claude's format for
// an Edit or Write tool call. This is the format ExtractClaudeActions parses.
func claudePayload(toolName, filePath, content string) []byte {
	// Minimal Claude payload: {"type":"assistant","message":{"content":[{"type":"tool_use","name":"...","input":{...}}]}}
	switch toolName {
	case "Write":
		return []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` +
			filePath + `","content":` + jsonEscape(content) + `}}]}}`)
	case "Edit":
		return []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"` +
			filePath + `","new_string":` + jsonEscape(content) + `}}]}}`)
	case "Bash":
		return []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":` +
			jsonEscape(content) + `}}]}}`)
	default:
		return nil
	}
}

// claudeEvent builds a Claude assistant EventRow with a Write/Edit/Bash payload.
// Sets PayloadHash to a non-empty sentinel so BuildCandidatesFromRows processes it.
func claudeEvent(toolName, filePath, content, repoRoot string) events.EventRow {
	var toolUses string
	switch toolName {
	case "Write":
		toolUses = `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"` + filePath + `","file_op":"write"}]}`
	case "Edit":
		toolUses = `{"content_types":["tool_use"],"tools":[{"name":"Edit","file_path":"` + filePath + `","file_op":"edit"}]}`
	case "Bash":
		toolUses = `{"content_types":["tool_use"],"tools":[{"name":"Bash","file_op":"exec"}]}`
	}

	absPath := filePath
	if repoRoot != "" && len(filePath) > 0 && filePath[0] != '/' {
		absPath = repoRoot + "/" + filePath
	}

	return events.EventRow{
		Provider:    "claude_code",
		Role:        "assistant",
		ToolUses:    toolUses,
		PayloadHash: "eval-fixture", // non-empty so BuildCandidatesFromRows processes the payload
		Payload:     claudePayload(toolName, absPath, content),
		Model:       "opus-4",
	}
}

// kiroIDEEvent builds a Kiro IDE assistant EventRow. ToolUses comes from the
// production agentKiro.BuildToolUsesJSON helper so this fixture follows the
// same payload shape as runtime events.
//
// This helper does not choose the tool-name constant for each action type;
// buildEventForOp tests cover that provider-level mapping. The corpus case
// covers the scoring pipeline: canonical Write/Edit tool_uses should route
// to line-level attribution rather than file-touch attribution.
func kiroIDEEvent(toolName, filePath, content, repoRoot string) events.EventRow {
	fileOp := "write"
	if toolName == "Edit" {
		fileOp = "edit"
	}
	toolUses := agentKiro.BuildToolUsesJSON(toolName, filePath, fileOp).String

	absPath := filePath
	if repoRoot != "" && len(filePath) > 0 && filePath[0] != '/' {
		absPath = repoRoot + "/" + filePath
	}

	return events.EventRow{
		Provider:    "kiro-ide",
		Role:        "assistant",
		ToolUses:    toolUses,
		PayloadHash: "eval-fixture",
		Payload:     claudePayload(toolName, absPath, content),
		Model:       "kiro-default",
	}
}

func jsonEscape(s string) string {
	// Simple JSON string escaping for test fixtures.
	out := `"`
	for _, c := range s {
		switch c {
		case '"':
			out += `\"`
		case '\\':
			out += `\\`
		case '\n':
			out += `\n`
		case '\t':
			out += `\t`
		default:
			out += string(c)
		}
	}
	return out + `"`
}

// Corpus is the evaluation case set. Start small (5 cases), scale after
// the schema proves correct.
var Corpus = []EvalCase{

	// Case 1: Claude exact-only happy path.
	// All lines match exactly. No normalization or overlap needed.
	{
		Name:        "claude-exact-only",
		Description: "Claude writes two new files, all lines match exactly",
		RepoRoot:    "/repo",
		Diff: "diff --git a/main.go b/main.go\n" +
			"--- /dev/null\n" +
			"+++ b/main.go\n" +
			"@@ -0,0 +1,3 @@\n" +
			"+package main\n" +
			"+\n" +
			"+func main() {}\n" +
			"diff --git a/util.go b/util.go\n" +
			"--- /dev/null\n" +
			"+++ b/util.go\n" +
			"@@ -0,0 +1,2 @@\n" +
			"+package main\n" +
			"+func helper() {}\n",
		Events: []events.EventRow{
			claudeEvent("Write", "main.go", "package main\n\nfunc main() {}\n", "/repo"),
			claudeEvent("Write", "util.go", "package main\nfunc helper() {}\n", "/repo"),
		},
		Expected: ExpectedResult{
			AIPercentage:  100,
			Evidence:      "High",
			FallbackCount: 0,
			Files: []ExpectedFile{
				{
					Path: "main.go", AILines: 2, HumanLines: 0,
					PrimaryEvidence:      reporting.EvidenceExact,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceExact},
				},
				{
					Path: "util.go", AILines: 2, HumanLines: 0,
					PrimaryEvidence:      reporting.EvidenceExact,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceExact},
				},
			},
		},
	},

	// Case 2: Formatter churn. Claude wrote "func foo(){" but a formatter
	// changed it to "func foo() {". Needs normalized matching.
	{
		Name:        "formatter-churn-normalized",
		Description: "Claude writes code, formatter changes whitespace, normalized matches needed",
		RepoRoot:    "/repo",
		Diff: "diff --git a/handler.go b/handler.go\n" +
			"--- /dev/null\n" +
			"+++ b/handler.go\n" +
			"@@ -0,0 +1,3 @@\n" +
			"+package api\n" +
			"+func Handle() {\n" +
			"+}\n",
		Events: []events.EventRow{
			// Claude wrote "func Handle(){" (no space before brace) but formatter added the space.
			claudeEvent("Write", "handler.go", "package api\nfunc Handle(){\n}\n", "/repo"),
		},
		Expected: ExpectedResult{
			AIPercentage:  100,
			Evidence:      "High",
			FallbackCount: 0,
			Files: []ExpectedFile{
				{
					Path: "handler.go", AILines: 3, HumanLines: 0,
					PrimaryEvidence: reporting.EvidenceExact,
					ContributingEvidence: []reporting.EvidenceClass{
						reporting.EvidenceExact,
						reporting.EvidenceNormalized,
					},
					Notes: "package api and } match exactly; func Handle() { matches via normalization",
				},
			},
		},
	},

	// Case 3: Provider file-touch only. Cursor edited a file but we have
	// no line-level payload. All lines become modified via provider-touch path.
	{
		Name:        "cursor-file-touch-only",
		Description: "Cursor edits a file, no line-level evidence, file-touch fallback",
		RepoRoot:    "/repo",
		Diff: "diff --git a/config.go b/config.go\n" +
			"--- /dev/null\n" +
			"+++ b/config.go\n" +
			"@@ -0,0 +1,3 @@\n" +
			"+package main\n" +
			"+var port = 8080\n" +
			"+var host = \"localhost\"\n",
		Events: []events.EventRow{
			{
				Provider: "cursor", Role: "assistant",
				ToolUses: `{"content_types":["cursor_file_edit"],"tools":[{"name":"cursor_file_edit","file_path":"config.go","file_op":"edit"}]}`,
				// No payload - Cursor doesn't send line-level content.
			},
		},
		Expected: ExpectedResult{
			AIPercentage:  100,
			Evidence:      "Medium",
			FallbackCount: 0,
			Files: []ExpectedFile{
				{
					Path: "config.go", AILines: 3, HumanLines: 0,
					PrimaryEvidence:      reporting.EvidenceModified,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceModified},
					Notes: "All lines become ModifiedLines via provider-touch-only path in ScoreFiles. " +
						"Primary evidence is 'modified' (not fallback) because ScoreFiles actually scores the lines.",
				},
			},
		},
	},

	// Case 4: Human-only file alongside AI file. The human file should get
	// EvidenceNone and not inflate the fallback count.
	{
		Name:        "mixed-human-and-ai",
		Description: "One AI file (exact) and one human-only file in the same commit",
		RepoRoot:    "/repo",
		Diff: "diff --git a/ai.go b/ai.go\n" +
			"--- /dev/null\n" +
			"+++ b/ai.go\n" +
			"@@ -0,0 +1,2 @@\n" +
			"+package main\n" +
			"+func generated() {}\n" +
			"diff --git a/human.go b/human.go\n" +
			"--- /dev/null\n" +
			"+++ b/human.go\n" +
			"@@ -0,0 +1,2 @@\n" +
			"+package main\n" +
			"+func handwritten() {}\n",
		Events: []events.EventRow{
			claudeEvent("Write", "ai.go", "package main\nfunc generated() {}\n", "/repo"),
			// No events for human.go.
		},
		Expected: ExpectedResult{
			AIPercentage:  50,
			Evidence:      "High",
			FallbackCount: 0,
			Files: []ExpectedFile{
				{
					Path: "ai.go", AILines: 2, HumanLines: 0,
					PrimaryEvidence:      reporting.EvidenceExact,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceExact},
				},
				{
					Path: "human.go", AILines: 0, HumanLines: 2,
					PrimaryEvidence:      reporting.EvidenceNone,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceNone},
				},
			},
		},
	},

	// Case 5: Group overlap causes modified attribution. Claude wrote one
	// line in a group of three. The other two unmatched lines in the same
	// group become AI-Modified because the group has overlap.
	{
		Name:        "group-overlap-modified",
		Description: "One exact match in a group promotes neighbors to AI-Modified",
		RepoRoot:    "/repo",
		Diff: "diff --git a/service.go b/service.go\n" +
			"--- /dev/null\n" +
			"+++ b/service.go\n" +
			"@@ -0,0 +1,3 @@\n" +
			"+func Start() {\n" +
			"+\tlog.Println(\"starting\")\n" +
			"+\tgo run()\n",
		Events: []events.EventRow{
			// Claude only wrote "func Start() {" -- the other two lines are human edits
			// within the same contiguous group.
			claudeEvent("Write", "service.go", "func Start() {\n", "/repo"),
		},
		Expected: ExpectedResult{
			AIPercentage:  100,
			Evidence:      "Medium",
			FallbackCount: 0,
			Files: []ExpectedFile{
				{
					Path: "service.go", AILines: 3, HumanLines: 0,
					PrimaryEvidence: reporting.EvidenceExact,
					ContributingEvidence: []reporting.EvidenceClass{
						reporting.EvidenceExact,
						reporting.EvidenceModified,
					},
					Notes: "1 exact + 2 modified from group overlap. Primary evidence is 'exact' " +
						"(highest wins). Evidence is Medium because LineScore = 0.7 " +
						"(modified lines pull it below the 0.75 High threshold).",
				},
			},
		},
	},

	// Case 6: Carry-forward success. A file was created and has matching
	// AI lines from a historical window. The CarryForwardFiles flag is set
	// because the orchestrator identified it as carry-forward eligible AND
	// the historical lookup produced AI lines.
	{
		Name:        "carry-forward-success",
		Description: "File attributed via carry-forward with actual AI lines from historical window",
		RepoRoot:    "/repo",
		Diff: "diff --git a/utils.go b/utils.go\n" +
			"--- /dev/null\n" +
			"+++ b/utils.go\n" +
			"@@ -0,0 +1,2 @@\n" +
			"+package utils\n" +
			"+func Helper() {}\n",
		Events: []events.EventRow{
			claudeEvent("Write", "utils.go", "package utils\nfunc Helper() {}\n", "/repo"),
		},
		CarryForwardFiles: map[string]bool{"utils.go": true},
		Expected: ExpectedResult{
			AIPercentage:  100,
			Evidence:      "High",
			FallbackCount: 0,
			Files: []ExpectedFile{
				{
					Path: "utils.go", AILines: 2, HumanLines: 0,
					PrimaryEvidence: reporting.EvidenceExact,
					ContributingEvidence: []reporting.EvidenceClass{
						reporting.EvidenceExact,
						reporting.EvidenceCarryForward,
					},
					Notes: "Exact line match wins for display, but carry-forward is a contributing " +
						"class because the lines came from a historical window.",
				},
			},
		},
	},

	// Case 7: Carry-forward attempt with zero score. The file was eligible
	// for carry-forward, but neither the current nor historical window had
	// matching AI candidates. The file should be human-only with no
	// carry-forward evidence.
	{
		Name:        "carry-forward-zero-score",
		Description: "Carry-forward attempted but no AI lines found in either window",
		RepoRoot:    "/repo",
		Diff: "diff --git a/config.yaml b/config.yaml\n" +
			"--- /dev/null\n" +
			"+++ b/config.yaml\n" +
			"@@ -0,0 +1,2 @@\n" +
			"+port: 8080\n" +
			"+host: localhost\n",
		Events: []events.EventRow{
			// An assistant event exists but with completely different content.
			claudeEvent("Write", "other.go", "package other\n", "/repo"),
		},
		// CarryForwardFiles is NOT set because the orchestrator would have
		// narrowed it to files that actually scored AI lines (actualCF).
		Expected: ExpectedResult{
			AIPercentage:  0,
			Evidence:      "",
			FallbackCount: 0,
			Files: []ExpectedFile{
				{
					Path: "config.yaml", AILines: 0, HumanLines: 2,
					PrimaryEvidence:      reporting.EvidenceNone,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceNone},
					Notes: "File was carry-forward eligible but scored zero AI lines. " +
						"The orchestrator excluded it from CarryForwardFiles (actualCF).",
				},
			},
		},
	},

	// Case 8: Bash deletion. Claude ran `rm old.go` in a bash command.
	// The deleted file appears in FilesDeleted but has no added lines
	// in the diff, so it doesn't go through scoring.
	{
		Name:        "bash-deletion-inference",
		Description: "File deleted via Claude bash command, tracked in FilesDeleted",
		RepoRoot:    "/repo",
		Diff: "diff --git a/old.go b/old.go\n" +
			"--- a/old.go\n" +
			"+++ /dev/null\n" +
			"@@ -1,2 +0,0 @@\n" +
			"-package main\n" +
			"-func deprecated() {}\n",
		Events: []events.EventRow{
			claudeEvent("Bash", "", "rm /repo/old.go", "/repo"),
		},
		Expected: ExpectedResult{
			AIPercentage:  0,     // deleted files have 0 added lines
			Evidence:      "Low", // LineScore=0, penalty=0.35 -> score=0
			FallbackCount: 1,     // deletion file counts as fallback
			Files: []ExpectedFile{
				{
					Path: "old.go", AILines: 0, HumanLines: 0,
					PrimaryEvidence:      reporting.EvidenceDeletion,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceDeletion},
					Notes: "File has zero added lines but is in the AI-touched set via bash deletion. " +
						"Evidence class is 'deletion' (inferential). The file appears in both " +
						"Files (with zero lines) and FilesDeleted in the commit result.",
				},
			},
		},
	},

	// Case 9: Kiro IDE line-level via canonical Write/Edit tool_uses.
	// A row carrying canonical Write/Edit tool_uses should flow through
	// BuildCandidatesFromRows and ScoreFiles as exact line-level
	// attribution, not the file-touch path. Provider action-to-tool-name
	// mapping is covered by buildEventForOp tests in the kiroide package.
	{
		Name:        "kiro-ide-line-level",
		Description: "Kiro IDE Write (create) and Edit (replace) emit canonical tool_uses; lines score line-level",
		RepoRoot:    "/repo",
		Diff: "diff --git a/main.go b/main.go\n" +
			"--- /dev/null\n" +
			"+++ b/main.go\n" +
			"@@ -0,0 +1,2 @@\n" +
			"+package main\n" +
			"+func main() {}\n" +
			"diff --git a/util.go b/util.go\n" +
			"--- a/util.go\n" +
			"+++ b/util.go\n" +
			"@@ -1,1 +1,2 @@\n" +
			" package main\n" +
			"+func helper() {}\n",
		Events: []events.EventRow{
			kiroIDEEvent("Write", "main.go", "package main\nfunc main() {}\n", "/repo"),
			kiroIDEEvent("Edit", "util.go", "package main\nfunc helper() {}\n", "/repo"),
		},
		Expected: ExpectedResult{
			AIPercentage:  100,
			Evidence:      "High",
			FallbackCount: 0,
			Files: []ExpectedFile{
				{
					Path: "main.go", AILines: 2, HumanLines: 0,
					PrimaryEvidence:      reporting.EvidenceExact,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceExact},
				},
				{
					Path: "util.go", AILines: 1, HumanLines: 0,
					PrimaryEvidence:      reporting.EvidenceExact,
					ContributingEvidence: []reporting.EvidenceClass{reporting.EvidenceExact},
				},
			},
		},
	},
}
