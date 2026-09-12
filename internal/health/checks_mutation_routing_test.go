package health

import (
	"strings"
	"testing"
)

func TestClassifyMutationRouting(t *testing.T) {
	tests := []struct {
		name         string
		toolUses     string
		wantMutation bool
		wantHasPath  bool
	}{
		{
			name:         "shell exec is mutation-capable and has no structured path",
			toolUses:     `{"content_types":["tool_use"],"tools":[{"name":"Bash","file_op":"exec"}]}`,
			wantMutation: true,
			wantHasPath:  false,
		},
		{
			name:         "write with absolute path is path-bearing",
			toolUses:     `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"/repo/a.go","file_op":"write"}]}`,
			wantMutation: true,
			wantHasPath:  true,
		},
		{
			name:         "edit with relative path is path-bearing",
			toolUses:     `{"content_types":["tool_use"],"tools":[{"name":"Edit","file_path":"internal/a.go","file_op":"edit"}]}`,
			wantMutation: true,
			wantHasPath:  true,
		},
		{
			name:         "read tool is not mutation-capable",
			toolUses:     `{"content_types":["tool_use"],"tools":[{"name":"Read","file_path":"/repo/a.go","file_op":"read"}]}`,
			wantMutation: false,
		},
		{
			name:         "delete is mutation-capable",
			toolUses:     `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"/repo/a.go","file_op":"delete"}]}`,
			wantMutation: true,
			wantHasPath:  true,
		},
		{
			name:         "malformed json is not mutation-capable",
			toolUses:     `{"tools":`,
			wantMutation: false,
		},
		{
			name:         "mixed tools: any mutation op counts, any path counts",
			toolUses:     `{"tools":[{"name":"Read","file_op":"read"},{"name":"Bash","file_op":"exec"}]}`,
			wantMutation: true,
			wantHasPath:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutation, hasPath := classifyMutationRouting(tt.toolUses)
			if mutation != tt.wantMutation {
				t.Fatalf("mutation = %v, want %v", mutation, tt.wantMutation)
			}
			if mutation && hasPath != tt.wantHasPath {
				t.Fatalf("hasPath = %v, want %v", hasPath, tt.wantHasPath)
			}
		})
	}
}

// A pathless mutation event (e.g. a Bash tool window) may still be captured in
// the command's target repository via observed changes. The diagnostic must
// report structured-path coverage only: it must not label such events as a
// launch-directory fallback, a misattribution, or an attribution fault.
func TestMutationRoutingResult_PathlessIsNotPresentedAsFault(t *testing.T) {
	all := mutationRoutingCounts{total: 100, withPath: 21, withoutPath: 79}
	recent := mutationRoutingCounts{total: 30, withPath: 5, withoutPath: 25}

	check := mutationRoutingResult(all, recent)

	if check.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK (informational, not a fault)", check.Status)
	}
	if check.Remediation != "" {
		t.Fatalf("remediation = %q, want empty (pathlessness alone is not a fault)", check.Remediation)
	}
	if !strings.Contains(check.Message, "without structured paths") {
		t.Fatalf("message %q should describe structured-path coverage", check.Message)
	}
	for _, banned := range []string{"fallback", "launch-directory", "launch directory", "misattribut", "fault"} {
		if strings.Contains(strings.ToLower(check.Message), banned) {
			t.Fatalf("message %q must not claim %q; pathless != fallback", check.Message, banned)
		}
	}
}

func TestMutationRoutingResult_NoEvents(t *testing.T) {
	check := mutationRoutingResult(mutationRoutingCounts{}, mutationRoutingCounts{})
	if check.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK", check.Status)
	}
	if strings.Contains(check.Message, "%") {
		t.Fatalf("empty-data message should not report a percentage: %q", check.Message)
	}
}
