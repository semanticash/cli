package skills

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/version"
)

// Archive limits bound network, disk, and extraction work.
const (
	maxArchiveBytes int64 = 16 * 1024 * 1024 // 16 MB compressed
	maxExtractBytes int64 = 64 * 1024 * 1024 // 64 MB uncompressed
	maxArchiveFiles       = 1000
	fetchTimeout          = 30 * time.Second
)

// skillsArchiveURL points to the published skills branch.
const skillsArchiveURL = "https://codeload.github.com/semanticash/skills/tar.gz/refs/heads/main"

// archiveURLResolver lets tests replace the fixed production URL.
var archiveURLResolver = defaultArchiveURL

func defaultArchiveURL() string { return skillsArchiveURL }

// ErrSkillsFetchFailed identifies download and extraction failures.
var ErrSkillsFetchFailed = errors.New("could not fetch skills from github.com; check your network or use --source <path> for offline install")

// fetchSkillsArchive downloads and extracts the published skills archive.
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

// downloadArchive returns a timeout- and size-limited response body.
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
	return &cancelOnCloseReader{
		ReadCloser: capReadCloser(resp.Body, maxArchiveBytes),
		cancel:     cancel,
	}, nil
}

// capReadCloser rejects reads beyond maxBytes.
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

// safeExtractTarGz extracts bounded regular files and directories beneath destRoot.
func safeExtractTarGz(r io.Reader, destRoot string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()
	root, err := os.OpenRoot(destRoot)
	if err != nil {
		return fmt.Errorf("open extraction root: %w", err)
	}
	defer func() { _ = root.Close() }()

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

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		default:
			continue
		}

		name := hdr.Name
		if hdr.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !fs.ValidPath(name) || strings.Contains(name, `\`) {
			return fmt.Errorf("archive entry has unsafe path: %q", hdr.Name)
		}
		localName, err := filepath.Localize(name)
		if err != nil || !filepath.IsLocal(localName) {
			return fmt.Errorf("archive entry has unsafe path: %q", hdr.Name)
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := root.MkdirAll(localName, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", localName, err)
			}
			continue
		}

		if err := root.MkdirAll(filepath.Dir(localName), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", localName, err)
		}
		remaining := maxExtractBytes - totalBytes
		if remaining <= 0 {
			return fmt.Errorf("archive uncompressed size exceeds %d bytes", maxExtractBytes)
		}
		f, err := root.OpenFile(localName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create %s: %w", localName, err)
		}
		// The extra byte distinguishes overflow from an exact fit.
		n, err := io.CopyN(f, tr, remaining+1)
		_ = f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("copy %s: %w", localName, err)
		}
		totalBytes += n
		if n > remaining {
			return fmt.Errorf("archive uncompressed size exceeds %d bytes", maxExtractBytes)
		}
	}
}

// findSkillsRoot locates skills/ beneath the archive's top-level directory.
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
