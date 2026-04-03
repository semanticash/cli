package llm

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildCommitMsgDiffContextPrioritizesCodeOverLargeDocDeletion(t *testing.T) {
	diff := buildDeletedFileDiff("PLAN_HOOK_BASED_CAPTURE.md", 200) + "\n" +
		`diff --git a/internal/service/suggest.go b/internal/service/suggest.go
index 1111111..2222222 100644
--- a/internal/service/suggest.go
+++ b/internal/service/suggest.go
@@ -1,3 +1,6 @@
 func Suggest() {
-    return oldValue
+    summary := buildSummary()
+    excerpt := buildExcerpt()
+    return summary + excerpt
 }
`

	summary, excerpt := buildCommitMsgDiffContext(diff)

	if !strings.Contains(summary, "Files changed: 2") {
		t.Fatalf("summary missing file count:\n%s", summary)
	}
	if !strings.Contains(summary, "internal/service/suggest.go") {
		t.Fatalf("summary missing code file:\n%s", summary)
	}
	if strings.Contains(excerpt, "PLAN_HOOK_BASED_CAPTURE.md") {
		t.Fatalf("excerpt should prefer code files when docs are also present:\n%s", excerpt)
	}
	if !strings.Contains(excerpt, "internal/service/suggest.go") {
		t.Fatalf("excerpt missing code file:\n%s", excerpt)
	}
}

func TestBuildCommitMsgPromptIncludesSummaryAndExcerpt(t *testing.T) {
	diff := `diff --git a/internal/hooks/provider.go b/internal/hooks/provider.go
index 1111111..2222222 100644
--- a/internal/hooks/provider.go
+++ b/internal/hooks/provider.go
@@ -1,3 +1,4 @@
+// HookProvider documents provider integration points.
 type HookProvider interface {}
`

	prompt := BuildCommitMsgPrompt(nil, diff)
	if !strings.Contains(prompt, "<change_summary>") {
		t.Fatalf("prompt missing change summary block:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<diff_excerpt>") {
		t.Fatalf("prompt missing diff excerpt block:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Representative files:") {
		t.Fatalf("prompt missing representative files summary:\n%s", prompt)
	}
	if !strings.Contains(prompt, "internal/hooks/provider.go") {
		t.Fatalf("prompt missing changed file path:\n%s", prompt)
	}
	if !strings.Contains(prompt, "you may return two short") {
		t.Fatalf("prompt missing optional second-sentence guidance:\n%s", prompt)
	}
}

func TestBuildCommitMsgDiffContextFallsBackToDocsWhenOnlyDocsChange(t *testing.T) {
	diff := buildDeletedFileDiff("README.md", 20)

	_, excerpt := buildCommitMsgDiffContext(diff)
	if !strings.Contains(excerpt, "README.md") {
		t.Fatalf("excerpt should include doc files when they are the only changes:\n%s", excerpt)
	}
}

func buildDeletedFileDiff(path string, lines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
	b.WriteString("deleted file mode 100644\n")
	fmt.Fprintf(&b, "--- a/%s\n", path)
	b.WriteString("+++ /dev/null\n")
	fmt.Fprintf(&b, "@@ -1,%d +0,0 @@\n", lines)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "-line %03d\n", i)
	}
	return b.String()
}
