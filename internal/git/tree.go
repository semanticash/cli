package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ObjectFormat returns the repository's Git object format ("sha1" or "sha256").
func (r *Repo) ObjectFormat(ctx context.Context) (string, error) {
	out, err := r.gitCmd(ctx, "rev-parse", "--show-object-format").Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("git rev-parse --show-object-format failed: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git rev-parse --show-object-format failed: %w", err)
	}
	format := strings.TrimSpace(string(out))
	if format != "sha1" && format != "sha256" {
		return "", fmt.Errorf("unsupported git object format %q", format)
	}
	return format, nil
}

// TreeEntry describes a leaf in a commit tree.
type TreeEntry struct {
	Mode     string // e.g. 100644, 100755, 120000, 160000
	Type     string // blob | commit (gitlink)
	ObjectID string // Git object ID
	Path     string // Git path, slash-separated, byte-preserving
}

// LsTreeEntries returns the tracked entries in a commit tree. Paths retain
// Git's separators and must be valid UTF-8. commit must be a full object ID.
func (r *Repo) LsTreeEntries(ctx context.Context, commit string) ([]TreeEntry, error) {
	if !IsFullCommitHash(commit) {
		return nil, fmt.Errorf("ls-tree: want a full commit hash, got %q", commit)
	}
	out, err := r.gitCmd(ctx, "ls-tree", "-r", "-z", commit).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git ls-tree %s failed: %w: %s", commit, err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git ls-tree %s failed: %w", commit, err)
	}
	return parseLsTreeOutput(out)
}

// parseLsTreeOutput parses NUL-terminated `git ls-tree -r -z` output. The first
// tab in each record separates metadata from the path. Empty, truncated, and
// non-UTF-8 paths are rejected.
func parseLsTreeOutput(out []byte) ([]TreeEntry, error) {
	if len(out) == 0 {
		return nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, fmt.Errorf("ls-tree: output is not NUL-terminated (truncated)")
	}
	// A NUL-terminated buffer splits into records plus a trailing empty element.
	records := bytes.Split(out, []byte{0})
	records = records[:len(records)-1]

	entries := make([]TreeEntry, 0, len(records))
	for _, rec := range records {
		if len(rec) == 0 {
			return nil, fmt.Errorf("ls-tree: empty record")
		}
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("ls-tree: record without tab: %q", rec)
		}
		fields := strings.Fields(string(rec[:tab]))
		if len(fields) != 3 {
			return nil, fmt.Errorf("ls-tree: malformed metadata %q", rec[:tab])
		}
		path := string(rec[tab+1:])
		if path == "" || !utf8.ValidString(path) {
			return nil, fmt.Errorf("ls-tree: empty or non-UTF-8 path")
		}
		entries = append(entries, TreeEntry{
			Mode:     fields[0],
			Type:     fields[1],
			ObjectID: fields[2],
			Path:     path,
		})
	}
	return entries, nil
}

// maxCatFileObjectSize bounds allocations from untrusted batch headers.
const maxCatFileObjectSize = 2 << 30 // 2 GiB

// CatFileBatch streams the raw contents of the given Git object IDs through fn,
// in request order, using one `git cat-file --batch` process. The callback must
// copy content if it retains it. Invalid responses and callback errors abort
// the batch.
func (r *Repo) CatFileBatch(ctx context.Context, oids []string, fn func(oid string, content []byte) error) error {
	if len(oids) == 0 {
		return nil
	}
	for _, oid := range oids {
		if !IsFullCommitHash(oid) {
			return fmt.Errorf("git cat-file: invalid object id %q", oid)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := r.gitCmd(ctx, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("git cat-file: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git cat-file: stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git cat-file --batch: start: %w", err)
	}

	// Feed requests from a goroutine so a full stdout pipe cannot deadlock the
	// reader below.
	writeDone := make(chan error, 1)
	go func() {
		w := bufio.NewWriter(stdin)
		var werr error
		for _, oid := range oids {
			if _, e := w.WriteString(oid + "\n"); e != nil {
				werr = e
				break
			}
		}
		if werr == nil {
			werr = w.Flush()
		}
		if ce := stdin.Close(); werr == nil {
			werr = ce
		}
		writeDone <- werr
	}()

	readErr := readCatFileBatch(bufio.NewReader(stdout), oids, fn)
	if readErr != nil {
		// Stop Git and unblock a writer that may be stuck on a full pipe.
		cancel()
	}
	writeErr := <-writeDone
	waitErr := cmd.Wait()

	switch {
	case readErr != nil:
		return fmt.Errorf("git cat-file: %w", readErr)
	case writeErr != nil:
		return fmt.Errorf("git cat-file: write requests: %w", writeErr)
	case waitErr != nil:
		return fmt.Errorf("git cat-file --batch: %w: %s", waitErr, stderr.String())
	}
	return nil
}

// readCatFileBatch validates batch responses and invokes fn in request order.
func readCatFileBatch(br *bufio.Reader, oids []string, fn func(oid string, content []byte) error) error {
	for _, oid := range oids {
		header, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read header for %s: %w", oid, err)
		}
		fields := strings.Fields(header)
		// "<oid> missing" / "<oid> ambiguous" carry two fields.
		if len(fields) == 2 {
			return fmt.Errorf("object %s is %s", oid, fields[1])
		}
		if len(fields) != 3 {
			return fmt.Errorf("malformed header %q", header)
		}
		if fields[0] != oid {
			return fmt.Errorf("response oid %q does not match request %q", fields[0], oid)
		}
		if !knownObjectType(fields[1]) {
			return fmt.Errorf("unexpected object type %q for %s", fields[1], oid)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("invalid size %q for %s", fields[2], oid)
		}
		if size > maxCatFileObjectSize {
			return fmt.Errorf("object %s size %d exceeds limit", oid, size)
		}
		content := make([]byte, size)
		if _, err := io.ReadFull(br, content); err != nil {
			return fmt.Errorf("read content for %s: %w", oid, err)
		}
		sep, err := br.ReadByte()
		if err != nil {
			return fmt.Errorf("read separator for %s: %w", oid, err)
		}
		if sep != '\n' {
			return fmt.Errorf("expected newline after %s content, got %q", oid, sep)
		}
		if err := fn(oid, content); err != nil {
			return err
		}
	}
	return nil
}

func knownObjectType(t string) bool {
	switch t {
	case "blob", "commit", "tree", "tag":
		return true
	}
	return false
}
