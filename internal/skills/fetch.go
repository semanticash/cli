package skills

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/version"
)

// Default download caps. The skills repo is small (well under
// 100 KB at the moment), so generous bounds still leave plenty of
// margin for growth without inviting DoS / disk-fill risk on a
// compromised or malformed archive.
const (
	maxArchiveBytes int64 = 16 * 1024 * 1024 // 16 MB compressed
	maxExtractBytes int64 = 64 * 1024 * 1024 // 64 MB uncompressed
	maxArchiveFiles       = 1000
	fetchTimeout          = 30 * time.Second
)

// skillsArchiveURL is the tarball endpoint for the protected main
// branch of semanticash/skills. Skills are prose assets; branch
// protection is the publish gate, and installed metadata handles
// ownership and version checks. If skills gain a compatibility
// surface, this resolver can switch to per-version pinning.
const skillsArchiveURL = "https://codeload.github.com/semanticash/skills/tar.gz/refs/heads/main"

// archiveURLResolver returns the URL to fetch the skills archive
// from. Production resolves to skillsArchiveURL; tests swap it
// for an httptest.Server URL. The variable seam is intentionally
// package-private: there is no documented public way to override
// the URL because we don't want users to point the installer at
// arbitrary tarballs.
var archiveURLResolver = defaultArchiveURL

func defaultArchiveURL() string { return skillsArchiveURL }

// ErrSkillsFetchFailed wraps any failure from the network or
// archive-extraction path so the install command can surface a
// single user-friendly error pointing at the offline `--source`
// escape hatch.
var ErrSkillsFetchFailed = errors.New("could not fetch skills from github.com; check your network or use --source <path> for offline install")

// fetchSkillsArchive downloads the skills archive at the resolved
// URL, extracts it into a temp directory under safe-extraction
// rules (no absolute paths, no traversal, no symlinks/hardlinks/
// devices, capped file count and uncompressed size), then locates
// the inner `skills/` directory and returns its path plus a
// cleanup function the caller defers. Both the network step and
// the extraction step are best-effort: any failure returns
// ErrSkillsFetchFailed wrapping the underlying cause so callers
// can render one consistent message.
func fetchSkillsArchive(ctx context.Context) (skillsRoot string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "semantica-skills-")
	if err != nil {
		return "", nil, fmt.Errorf("%w: create temp dir: %v", ErrSkillsFetchFailed, err)
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	body, err := downloadArchive(ctx, archiveURLResolver())
	if err != nil {
		return "", cleanup, err
	}
	defer func() { _ = body.Close() }()

	if err := safeExtractTarGz(body, tmp); err != nil {
		return "", cleanup, fmt.Errorf("%w: %v", ErrSkillsFetchFailed, err)
	}

	root, err := findSkillsRoot(tmp)
	if err != nil {
		return "", cleanup, fmt.Errorf("%w: %v", ErrSkillsFetchFailed, err)
	}
	return root, cleanup, nil
}

// downloadArchive performs the HTTP GET with timeout, asserts a
// 200 response, and returns the size-capped body reader.
func downloadArchive(ctx context.Context, url string) (io.ReadCloser, error) {
	reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrSkillsFetchFailed, err)
	}
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrSkillsFetchFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%w: HTTP %d from %s", ErrSkillsFetchFailed, resp.StatusCode, url)
	}
	// Wrap the body so the size cap is enforced as bytes are read,
	// and so closing the body also cancels the timeout context.
	return &cancelOnCloseReader{
		ReadCloser: capReadCloser(resp.Body, maxArchiveBytes),
		cancel:     cancel,
	}, nil
}

// capReadCloser wraps body with a hard byte limit. Reads beyond
// maxBytes return io.ErrUnexpectedEOF so the gzip reader fails
// loudly rather than silently truncating.
func capReadCloser(rc io.ReadCloser, maxBytes int64) io.ReadCloser {
	return &capReader{rc: rc, remaining: maxBytes}
}

type capReader struct {
	rc        io.ReadCloser
	remaining int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, fmt.Errorf("archive download exceeds %d bytes", maxArchiveBytes)
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.rc.Read(p)
	c.remaining -= int64(n)
	return n, err
}

func (c *capReader) Close() error { return c.rc.Close() }

type cancelOnCloseReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseReader) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// safeExtractTarGz reads a gzip+tar stream and writes regular
// files and directories under destRoot, with the following safety
// rules:
//
//   - Reject anything that isn't a regular file or directory
//     (symlinks, hardlinks, char/block devices, FIFOs are skipped
//     silently: neither writing them nor following any link
//     target).
//   - Reject absolute paths and `..` traversal in entry names.
//   - Verify each computed target stays under destRoot via
//     filepath.Rel before writing.
//   - Cap total uncompressed bytes at maxExtractBytes.
//   - Cap total entry count at maxArchiveFiles.
//
// These checks deliberately apply even to GitHub-sourced archives
// so the installer cannot become a tar-slip footgun if the source
// repo is ever compromised or hand-crafted.
func safeExtractTarGz(r io.Reader, destRoot string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	var totalBytes int64
	var fileCount int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar entry: %w", err)
		}

		fileCount++
		if fileCount > maxArchiveFiles {
			return fmt.Errorf("archive contains more than %d entries", maxArchiveFiles)
		}

		// Type filter: explicitly accept regular files and
		// directories; reject everything else (symlink, hardlink,
		// char/block device, FIFO).
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir:
			// proceed
		default:
			continue
		}

		// Tar paths use forward slashes. Reject both native
		// absolute paths and Unix-style leading slash paths so
		// archives cannot escape destRoot on any platform.
		if filepath.IsAbs(hdr.Name) || strings.HasPrefix(hdr.Name, "/") {
			return fmt.Errorf("archive entry has absolute path: %q", hdr.Name)
		}
		cleaned := filepath.Clean(hdr.Name)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry escapes root: %q", hdr.Name)
		}

		target := filepath.Join(destRoot, cleaned)
		rel, err := filepath.Rel(destRoot, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry escapes root: %q", hdr.Name)
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", target, err)
		}
		// Cap copy at maxExtractBytes - totalBytes; if we read past
		// that, fail loudly rather than silently truncating.
		remaining := maxExtractBytes - totalBytes
		if remaining <= 0 {
			return fmt.Errorf("archive uncompressed size exceeds %d bytes", maxExtractBytes)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}
		// Read one extra byte so we can detect overflow rather than
		// silently stopping at the cap.
		n, err := io.CopyN(f, tr, remaining+1)
		_ = f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("copy %s: %w", target, err)
		}
		totalBytes += n
		if n > remaining {
			return fmt.Errorf("archive uncompressed size exceeds %d bytes", maxExtractBytes)
		}
	}
}

// findSkillsRoot walks one level into the extracted tree and
// returns the path to the first `skills/` directory it finds.
// GitHub tarballs nest everything under a single top-level
// `<repo>-<sha>/` directory; the actual skills live one level
// deeper at `<repo>-<sha>/skills/`.
func findSkillsRoot(extracted string) (string, error) {
	entries, err := os.ReadDir(extracted)
	if err != nil {
		return "", fmt.Errorf("read extracted dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(extracted, e.Name(), "skills")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no skills/ directory found inside archive")
}
