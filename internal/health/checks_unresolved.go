package health

import (
	"fmt"
	"os"
	"strings"

	"github.com/semanticash/cli/internal/broker"
)

func checkUnresolvedMutations() []Check {
	root, err := broker.UnresolvedMutationDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	check := Check{Category: "capture", ID: "unresolved_mutations", Status: StatusWarn}
	if err != nil {
		check.Message = "unresolved mutation records unreadable: " + err.Error()
		return []Check{check}
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	check.Message = fmt.Sprintf("%d potential mutation event(s) retained with unresolved destinations (all repositories); includes shell commands without mutation paths", count)
	check.Remediation = "Records are retained in " + root + "; they are not automatically assigned to the session repository."
	return []Check{check}
}
