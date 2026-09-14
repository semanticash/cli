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
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return nil
	}
	check := Check{Category: "capture", ID: "unresolved_mutations", Status: StatusWarn}
	if err != nil {
		check.Message = "unresolved mutation archive inaccessible: " + err.Error()
		return []Check{check}
	}
	if !info.IsDir() {
		check.Message = "unresolved mutation archive is not a directory: " + root
		return []Check{check}
	}
	entries, err := os.ReadDir(root)
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
	check.Status = StatusOK
	check.Message = fmt.Sprintf("%d potential mutation event(s) retained with unresolved destinations (all repositories); includes shell commands without mutation paths", count)
	return []Check{check}
}
