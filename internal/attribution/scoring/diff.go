package scoring

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// ParseDiff extracts added lines, file operations, and new-file coordinates
// from a unified diff.
//
// It recognizes:
//   - "--- /dev/null" + "+++ b/path" -> file created
//   - "--- a/path" + "+++ /dev/null" -> file deleted
//   - Lines starting with "+" (excluding the +++ header) -> added lines
//
// Added groups record the first new-file line from their hunk header.
func ParseDiff(diffBytes []byte) DiffResult {
	var res DiffResult
	var current *FileDiff
	var currentOldPath string
	inAddedRun := false
	newLine := 0       // next new-file line number within the current hunk
	newLineOK := false // coordinates valid only after a well-formed header

	finalizeGroup := func() {
		if !inAddedRun || current == nil {
			inAddedRun = false
			return
		}
		inAddedRun = false
	}

	scanner := bufio.NewScanner(bytes.NewReader(diffBytes))
	// Surface oversized generated lines as an incomplete parse.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
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
			newLine, newLineOK = 0, false
			continue
		}

		if strings.HasPrefix(line, "@@") {
			finalizeGroup()
			newLine, newLineOK = hunkNewStart(line)
			continue
		}

		if strings.HasPrefix(line, "diff --git") ||
			strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "new file") || strings.HasPrefix(line, "deleted file") {
			finalizeGroup()
			continue
		}

		if strings.HasPrefix(line, `\`) {
			// "\ No newline at end of file" advances nothing.
			continue
		}

		if current != nil && strings.HasPrefix(line, "+") {
			if !inAddedRun {
				start := 0
				if newLineOK {
					start = newLine
				}
				current.Groups = append(current.Groups, AddedGroup{NewStart: start})
				inAddedRun = true
			}
			g := &current.Groups[len(current.Groups)-1]
			g.Lines = append(g.Lines, line[1:])
			if newLineOK {
				newLine++
			}
		} else if current != nil && strings.HasPrefix(line, "-") {
			finalizeGroup()
			if trimmed := strings.TrimSpace(line[1:]); trimmed != "" {
				current.DeletedNonBlank++
			}
		} else {
			finalizeGroup()
			if newLineOK {
				newLine++ // context line exists in the new file
			}
		}
	}

	res.Complete = scanner.Err() == nil
	return res
}

// hunkNewStart parses the new-file range from a standard hunk header.
func hunkNewStart(line string) (int, bool) {
	if !strings.HasPrefix(line, "@@ ") {
		return 0, false
	}
	rest := line[3:]
	end := strings.Index(rest, " @@")
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(rest[:end])
	if len(fields) != 2 ||
		!strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, false
	}
	if _, ok := parseHunkRange(fields[0][1:]); !ok {
		return 0, false
	}
	return parseHunkRange(fields[1][1:])
}

// parseHunkRange parses "start[,count]" and returns the start.
func parseHunkRange(s string) (int, bool) {
	start, count, hasCount := strings.Cut(s, ",")
	if !allDigits(start) || (hasCount && !allDigits(count)) {
		return 0, false
	}
	n, err := strconv.Atoi(start)
	if err != nil {
		return 0, false
	}
	if hasCount {
		if _, err := strconv.Atoi(count); err != nil {
			return 0, false
		}
	}
	return n, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
