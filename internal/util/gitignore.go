package util

import (
	"os"
	"path/filepath"
	"strings"
)

// EnsureGitignoreEntries appends any missing entries to the repo's .gitignore
// under a "# Semantica" section. Entries that already appear (with or without
// trailing slash for directories) are skipped.
func EnsureGitignoreEntries(repoRoot string, entries []string) error {
	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	data, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Determine which entries are missing.
	existing := make(map[string]bool)
	if data != nil {
		for _, line := range strings.Split(string(data), "\n") {
			existing[strings.TrimSpace(line)] = true
		}
	}

	var missing []string
	for _, e := range entries {
		trimmed := strings.TrimSuffix(e, "/")
		if !existing[e] && !existing[trimmed] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	// If file doesn't exist yet, create with header.
	if os.IsNotExist(err) {
		content := "# Semantica\n" + strings.Join(missing, "\n") + "\n"
		return os.WriteFile(gitignorePath, []byte(content), 0o644)
	}

	// Append safely.
	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}

	_, err = f.WriteString("\n# Semantica\n" + strings.Join(missing, "\n") + "\n")
	return err
}
