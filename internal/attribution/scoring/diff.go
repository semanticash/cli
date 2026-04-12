package scoring

import (
	"bufio"
	"bytes"
	"strings"
)

// ParseDiff parses a unified diff (as produced by "git diff") into per-file
// added lines, and identifies newly created and deleted files.
//
// It recognizes:
//   - "--- /dev/null" + "+++ b/path" -> file created
//   - "--- a/path" + "+++ /dev/null" -> file deleted
//   - Lines starting with "+" (excluding the +++ header) -> added lines
func ParseDiff(diffBytes []byte) DiffResult {
	var res DiffResult
	var current *FileDiff
	var currentOldPath string
	inAddedRun := false

	finalizeGroup := func() {
		if !inAddedRun || current == nil {
			inAddedRun = false
			return
		}
		inAddedRun = false
	}

	scanner := bufio.NewScanner(bytes.NewReader(diffBytes))
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "--- ") {
			finalizeGroup()
			currentOldPath = strings.TrimPrefix(line, "--- ")
			continue
		}

		if strings.HasPrefix(line, "+++ ") {
			finalizeGroup()
			newPath := strings.TrimPrefix(line, "+++ ")

			if currentOldPath == "/dev/null" && strings.HasPrefix(newPath, "b/") {
				path := strings.TrimPrefix(newPath, "b/")
				res.FilesCreated = append(res.FilesCreated, path)
			} else if newPath == "/dev/null" && strings.HasPrefix(currentOldPath, "a/") {
				path := strings.TrimPrefix(currentOldPath, "a/")
				res.FilesDeleted = append(res.FilesDeleted, path)
			}

			if strings.HasPrefix(newPath, "b/") {
				path := strings.TrimPrefix(newPath, "b/")
				res.Files = append(res.Files, FileDiff{Path: path})
				current = &res.Files[len(res.Files)-1]
			} else if newPath == "/dev/null" && strings.HasPrefix(currentOldPath, "a/") {
				path := strings.TrimPrefix(currentOldPath, "a/")
				res.Files = append(res.Files, FileDiff{Path: path})
				current = &res.Files[len(res.Files)-1]
			} else {
				current = nil
			}
			currentOldPath = ""
			continue
		}

		if strings.HasPrefix(line, "diff --git") ||
			strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "@@") ||
			strings.HasPrefix(line, "new file") || strings.HasPrefix(line, "deleted file") {
			finalizeGroup()
			continue
		}

		if current != nil && strings.HasPrefix(line, "+") {
			if !inAddedRun {
				current.Groups = append(current.Groups, AddedGroup{})
				inAddedRun = true
			}
			g := &current.Groups[len(current.Groups)-1]
			g.Lines = append(g.Lines, line[1:])
		} else if current != nil && strings.HasPrefix(line, "-") {
			finalizeGroup()
			if trimmed := strings.TrimSpace(line[1:]); trimmed != "" {
				current.DeletedNonBlank++
			}
		} else {
			finalizeGroup()
		}
	}

	return res
}
