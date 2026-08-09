package toolsnap

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// dirtyPaths returns every repository-relative path whose worktree
// content may differ from HEAD: modified, staged, type-changed,
// unmerged, renamed (both sides), and untracked non-ignored files.
//
// Status only nominates candidates; the worktree supplies file type,
// mode, and content. Parsing stops when the path limit is exceeded.
func dirtyPaths(ctx context.Context, rc RepoContext, limit int) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git",
		"status", "--porcelain=v2", "-z", "--untracked-files=all", "--no-renames")
	cmd.Dir = rc.WorktreeRoot
	cmd.Env = gitEnv(nil)
	platform.HideWindow(cmd)
	stderr := &boundedBuffer{limit: 8 * 1024}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("toolsnap: status pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("toolsnap: status: %w", err)
	}

	p := &statusParser{limit: limit, seen: map[string]bool{}}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	scanner.Split(scanNul)
	var perr error
	for scanner.Scan() {
		if perr = p.feed(scanner.Text()); perr != nil {
			break
		}
	}
	if perr == nil {
		perr = scanner.Err()
	}
	if perr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, perr
	}
	if err := cmd.Wait(); err != nil {
		if msg := stderr.String(); msg != "" {
			return nil, fmt.Errorf("toolsnap: status: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("toolsnap: status: %w", err)
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	return p.sortedPaths(), nil
}

// statusParser consumes NUL-delimited porcelain-v2 records. Unknown or
// malformed records fail closed.
type statusParser struct {
	limit      int
	seen       map[string]bool
	expectOrig bool
}

func (p *statusParser) feed(rec string) error {
	if p.expectOrig {
		// The record following a rename entry is its origin path.
		p.expectOrig = false
		if rec == "" {
			return malformedStatus(rec)
		}
		return p.add(rec)
	}
	if rec == "" {
		return nil
	}
	switch rec[0] {
	case '1': // ordinary changed entry: 8 space-separated fields, then path
		path, ok := fieldTail(rec, 8)
		if !ok {
			return malformedStatus(rec)
		}
		return p.add(path)
	case '2': // renamed/copied: 9 fields, then path; next record is origPath
		path, ok := fieldTail(rec, 9)
		if !ok {
			return malformedStatus(rec)
		}
		if err := p.add(path); err != nil {
			return err
		}
		p.expectOrig = true
		return nil
	case 'u': // unmerged: 10 fields, then path
		path, ok := fieldTail(rec, 10)
		if !ok {
			return malformedStatus(rec)
		}
		return p.add(path)
	case '?': // untracked
		if len(rec) < 3 {
			return malformedStatus(rec)
		}
		return p.add(rec[2:])
	case '#', '!': // headers and ignored entries carry no capture paths
		return nil
	default:
		return malformedStatus(rec)
	}
}

func (p *statusParser) add(path string) error {
	if isInternalPath(path) || p.seen[path] {
		return nil
	}
	p.seen[path] = true
	if p.limit > 0 && len(p.seen) > p.limit {
		return &PartialError{
			Reason: ReasonFileLimit,
			Detail: fmt.Sprintf("candidate paths exceed limit %d", p.limit),
		}
	}
	return nil
}

func (p *statusParser) finish() error {
	if p.expectOrig {
		return malformedStatus("missing rename origin path")
	}
	return nil
}

func (p *statusParser) sortedPaths() []string {
	paths := make([]string, 0, len(p.seen))
	for path := range p.seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// parseStatusRecords parses complete porcelain-v2 -z output without a
// path limit. Production parsing streams through statusParser.
func parseStatusRecords(out string) ([]string, error) {
	p := &statusParser{seen: map[string]bool{}}
	for _, rec := range strings.Split(out, "\x00") {
		if err := p.feed(rec); err != nil {
			return nil, err
		}
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	return p.sortedPaths(), nil
}

// boundedBuffer retains a fixed prefix of subprocess output.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	return strings.TrimSpace(b.buf.String())
}

func scanNul(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func malformedStatus(rec string) error {
	return &PartialError{Reason: ReasonMalformedStatus, Detail: fmt.Sprintf("unparseable status record %q", rec)}
}

// fieldTail returns the record content after n space-separated fields.
// Paths may contain spaces, so only the leading fields are split.
func fieldTail(rec string, n int) (string, bool) {
	rest := rec
	for i := 0; i < n; i++ {
		idx := strings.IndexByte(rest, ' ')
		if idx < 0 {
			return "", false
		}
		rest = rest[idx+1:]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

// isInternalPath excludes Semantica-owned and Git-internal paths from
// snapshot evidence.
func isInternalPath(p string) bool {
	return p == ".semantica" || strings.HasPrefix(p, ".semantica/") ||
		p == ".git" || strings.HasPrefix(p, ".git/")
}
