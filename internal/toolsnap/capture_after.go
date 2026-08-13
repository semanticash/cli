package toolsnap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// CaptureResult contains a post-tool snapshot and its bounded delta.
type CaptureResult struct {
	Post      Snapshot
	Files     []FileDelta
	BytesRead int64
	Truncated bool
}

// translateTimeout maps deadline expiry to the stable partial reason.
func translateTimeout(ctx context.Context, err error) error {
	var pe *PartialError
	if err == nil || errors.As(err, &pe) {
		return err
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &PartialError{Reason: ReasonTimeout, Detail: err.Error()}
	}
	return err
}

// CaptureAfter compares the post-tool workspace with a prior snapshot.
// Capture limits and unavailable evidence return typed partial errors.
func (s *Store) CaptureAfter(ctx context.Context, before Snapshot) (CaptureResult, error) {
	res, err := s.captureAfter(ctx, before)
	if err != nil {
		return CaptureResult{}, translateTimeout(ctx, err)
	}
	return res, nil
}

func (s *Store) captureAfter(ctx context.Context, before Snapshot) (CaptureResult, error) {
	// Resolve HEAD again because the pre-tool process may have exited.
	post, err := s.capture(ctx, false)
	if err != nil {
		return CaptureResult{}, err
	}
	if post.HeadHash != before.HeadHash {
		return CaptureResult{}, &PartialError{
			Reason: ReasonHeadChanged,
			Detail: fmt.Sprintf("HEAD moved from %s to %s during the window", before.HeadHash, post.HeadHash),
		}
	}
	if post.TreeHash == before.TreeHash {
		return CaptureResult{Post: post}, nil
	}
	files, bytesRead, truncated, err := s.DeltaBetweenTrees(ctx, before.TreeHash, post.TreeHash)
	if err != nil {
		return CaptureResult{}, err
	}
	return CaptureResult{Post: post, Files: files, BytesRead: bytesRead, Truncated: truncated}, nil
}

// DeltaBetweenTrees computes bounded file deltas between captured trees.
func (s *Store) DeltaBetweenTrees(ctx context.Context, beforeTree, afterTree string) ([]FileDelta, int64, bool, error) {
	if beforeTree == afterTree {
		return nil, 0, false, nil
	}
	changes, err := s.DiffTrees(ctx, beforeTree, afterTree)
	if err != nil {
		return nil, 0, false, err
	}
	if len(changes) > s.maxPaths() {
		return nil, 0, false, &PartialError{
			Reason: ReasonFileLimit,
			Detail: fmt.Sprintf("%d changed paths exceed limit %d", len(changes), s.maxPaths()),
		}
	}

	// Gitlinks reference subrepository commits and remain file-level evidence.
	seen := map[string]bool{}
	var hashes []string
	for _, c := range changes {
		if c.BeforeMode == gitlinkMode || c.AfterMode == gitlinkMode {
			continue
		}
		for _, h := range []string{c.BeforeHash, c.AfterHash} {
			if !isZeroHash(h) && !seen[h] {
				seen[h] = true
				hashes = append(hashes, h)
			}
		}
	}
	blobs, bytesRead, err := s.batchReadBlobs(ctx, hashes)
	if err != nil {
		return nil, 0, false, err
	}

	diffBudget := int64(maxDiffWorkPerCapture)
	truncated := false
	files := make([]FileDelta, 0, len(changes))
	for _, c := range changes {
		if ctx.Err() != nil {
			return nil, 0, false, &PartialError{
				Reason: ReasonTimeout,
				Detail: "capture deadline exceeded during delta generation",
			}
		}
		fd := FileDelta{
			Path:       c.Path,
			Operation:  operationForStatus(c.Op),
			BeforeHash: c.BeforeHash,
			AfterHash:  c.AfterHash,
			BeforeMode: c.BeforeMode,
			AfterMode:  c.AfterMode,
		}
		if c.BeforeMode == gitlinkMode || c.AfterMode == gitlinkMode {
			files = append(files, fd)
			continue
		}
		var beforeContent, afterContent []byte
		var beforeOK, afterOK = true, true
		if !isZeroHash(c.BeforeHash) {
			beforeContent, beforeOK = blobs[c.BeforeHash].content, blobs[c.BeforeHash].typ == "blob"
		}
		if !isZeroHash(c.AfterHash) {
			afterContent, afterOK = blobs[c.AfterHash].content, blobs[c.AfterHash].typ == "blob"
		}
		switch {
		case !beforeOK || !afterOK:
			// Non-blob objects remain file-level evidence.
		case isBinary(beforeContent) || isBinary(afterContent):
			fd.Binary = true
		case lineCount(beforeContent)+lineCount(afterContent) > maxDiffLinesPerFile:
			// Avoid materializing oversized line indexes.
			fd.Truncated = true
			truncated = true
		default:
			fd.Hunks, err = diffLinesBudget(ctx, beforeContent, afterContent, &diffBudget)
			if err != nil {
				return nil, 0, false, err
			}
			fd.OldNoEOFNewline = len(beforeContent) > 0 && beforeContent[len(beforeContent)-1] != '\n'
			fd.NewNoEOFNewline = len(afterContent) > 0 && afterContent[len(afterContent)-1] != '\n'
		}
		files = append(files, fd)
	}
	return files, bytesRead, truncated, nil
}

func operationForStatus(op byte) string {
	switch op {
	case 'A':
		return "create"
	case 'D':
		return "delete"
	case 'T':
		return "typechange"
	default:
		return "edit"
	}
}

func isZeroHash(h string) bool {
	for _, r := range h {
		if r != '0' {
			return false
		}
	}
	return len(h) > 0
}

// blobRecord is one object loaded from the snapshot store.
type blobRecord struct {
	content []byte
	typ     string
}

// batchReadBlobs streams deduplicated objects within the byte limit.
// Missing objects return alternate_object_missing without fetching.
func (s *Store) batchReadBlobs(ctx context.Context, hashes []string) (map[string]blobRecord, int64, error) {
	if len(hashes) == 0 {
		return nil, 0, nil
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", s.Dir, "cat-file", "--batch")
	cmd.Dir = filepath.Dir(s.Dir)
	cmd.Env = storeGitEnv(nil)
	platform.HideWindow(cmd)
	cmd.Stdin = strings.NewReader(strings.Join(hashes, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("toolsnap: batch pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("toolsnap: batch start: %w", err)
	}
	kill := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	reader := bufio.NewReaderSize(stdout, 64*1024)
	blobs := make(map[string]blobRecord, len(hashes))
	var total int64
	for i := 0; i < len(hashes); i++ {
		header, err := reader.ReadString('\n')
		if err != nil {
			kill()
			return nil, 0, fmt.Errorf("toolsnap: batch response %d of %d unreadable: %w", i+1, len(hashes), err)
		}
		fields := strings.Fields(strings.TrimSuffix(header, "\n"))
		// cat-file responses must match request order.
		if len(fields) >= 1 && fields[0] != hashes[i] {
			kill()
			return nil, 0, fmt.Errorf("toolsnap: batch response %q does not match requested %s", header, hashes[i])
		}
		if len(fields) == 2 && fields[1] == "missing" {
			kill()
			return nil, 0, &PartialError{
				Reason: ReasonAlternateGone,
				Detail: fmt.Sprintf("object %s is not reachable from the snapshot store", fields[0]),
			}
		}
		if len(fields) != 3 {
			kill()
			return nil, 0, fmt.Errorf("toolsnap: unexpected batch header %q", header)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			kill()
			return nil, 0, fmt.Errorf("toolsnap: unparseable object size in %q", header)
		}
		total += size
		if total > s.maxBytes() {
			kill()
			return nil, 0, &PartialError{
				Reason: ReasonByteLimit,
				Detail: fmt.Sprintf("delta content exceeds the %d byte limit", s.maxBytes()),
			}
		}
		content := make([]byte, size)
		if _, err := io.ReadFull(reader, content); err != nil {
			kill()
			return nil, 0, fmt.Errorf("toolsnap: truncated content for %s: %w", fields[0], err)
		}
		delim, err := reader.ReadByte()
		if err != nil || delim != '\n' {
			kill()
			return nil, 0, fmt.Errorf("toolsnap: missing delimiter after %s", fields[0])
		}
		blobs[fields[0]] = blobRecord{content: content, typ: fields[1]}
	}
	if err := cmd.Wait(); err != nil {
		return nil, 0, fmt.Errorf("toolsnap: batch read: %w", err)
	}
	if len(blobs) != len(hashes) {
		return nil, 0, fmt.Errorf("toolsnap: %d responses for %d requested objects", len(blobs), len(hashes))
	}
	return blobs, total, nil
}
