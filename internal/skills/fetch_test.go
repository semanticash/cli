package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- URL resolution ---

// TestDefaultArchiveURL_PointsAtProtectedMain checks the publish
// channel: installs fetch semanticash/skills from main. Branch
// protection gates published skill content; per-version pinning
// can be added here if skills gain a compatibility surface.
func TestDefaultArchiveURL_PointsAtProtectedMain(t *testing.T) {
	got := defaultArchiveURL()
	const wantSuffix = "/semanticash/skills/tar.gz/refs/heads/main"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("defaultArchiveURL() = %q, want suffix %q", got, wantSuffix)
	}
}

// --- HTTP-branch tests via httptest.Server ---

// TestInstall_FetchHappyPath covers the no-flag default flow: an
// httptest server returns a well-formed tar.gz containing one
// SKILL.md, the installer fetches it, extracts safely, stamps,
// and writes to the configured agent target.
func TestInstall_FetchHappyPath(t *testing.T) {
	disableAllAgentTargets(t)
	dst := filepath.Join(t.TempDir(), "claude-skills")
	t.Setenv(ClaudeSkillsDirEnv, dst)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(buildTarGz(t, []tarEntry{
			{Name: "skills-main/", Type: tar.TypeDir},
			{Name: "skills-main/skills/", Type: tar.TypeDir},
			{Name: "skills-main/skills/semantica-handoff/", Type: tar.TypeDir},
			{Name: "skills-main/skills/semantica-handoff/SKILL.md", Type: tar.TypeReg, Body: []byte(sampleSkill)},
		}))
	}))
	defer srv.Close()
	withArchiveURL(t, srv.URL)

	rep, err := Install(context.Background(), InstallOptions{CLIVersion: "v0.3.9"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(rep.Actions) != 1 || rep.Actions[0].Action != ActionInstalled {
		t.Fatalf("expected one ActionInstalled, got %+v", rep.Actions)
	}
	if _, err := os.Stat(filepath.Join(dst, "semantica-handoff", SkillFileName)); err != nil {
		t.Errorf("file not written: %v", err)
	}
}

func TestInstall_FetchNonOK(t *testing.T) {
	disableAllAgentTargets(t)
	t.Setenv(ClaudeSkillsDirEnv, filepath.Join(t.TempDir(), "claude-skills"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	withArchiveURL(t, srv.URL)

	_, err := Install(context.Background(), InstallOptions{CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrSkillsFetchFailed) {
		t.Errorf("expected ErrSkillsFetchFailed, got %v", err)
	}
}

func TestInstall_FetchBadGzip(t *testing.T) {
	disableAllAgentTargets(t)
	t.Setenv(ClaudeSkillsDirEnv, filepath.Join(t.TempDir(), "claude-skills"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not actually gzip"))
	}))
	defer srv.Close()
	withArchiveURL(t, srv.URL)

	_, err := Install(context.Background(), InstallOptions{CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrSkillsFetchFailed) {
		t.Errorf("expected ErrSkillsFetchFailed, got %v", err)
	}
}

func TestInstall_FetchMissingSkillsRoot(t *testing.T) {
	// A well-formed archive that doesn't contain a `skills/` dir.
	disableAllAgentTargets(t)
	t.Setenv(ClaudeSkillsDirEnv, filepath.Join(t.TempDir(), "claude-skills"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buildTarGz(t, []tarEntry{
			{Name: "wrong-layout/", Type: tar.TypeDir},
			{Name: "wrong-layout/notskills/", Type: tar.TypeDir},
		}))
	}))
	defer srv.Close()
	withArchiveURL(t, srv.URL)

	_, err := Install(context.Background(), InstallOptions{CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrSkillsFetchFailed) {
		t.Errorf("expected ErrSkillsFetchFailed for missing skills/, got %v", err)
	}
}

// --- Tar-extraction safety contracts ---

func TestSafeExtract_RejectsAbsolutePath(t *testing.T) {
	dest := t.TempDir()
	tgz := buildTarGz(t, []tarEntry{
		{Name: "/etc/evil", Type: tar.TypeReg, Body: []byte("nope")},
	})
	if err := safeExtractTarGz(bytes.NewReader(tgz), dest); err == nil {
		t.Errorf("expected error for absolute-path entry; extraction succeeded")
	}
	if _, err := os.Stat("/etc/evil"); err == nil {
		t.Errorf("absolute-path entry escaped the dest root!")
	}
}

func TestSafeExtract_RejectsTraversal(t *testing.T) {
	dest := t.TempDir()
	tgz := buildTarGz(t, []tarEntry{
		{Name: "../../../etc/evil", Type: tar.TypeReg, Body: []byte("nope")},
	})
	if err := safeExtractTarGz(bytes.NewReader(tgz), dest); err == nil {
		t.Errorf("expected error for ../ entry; extraction succeeded")
	}
}

// TestSafeExtract_SkipsSymlinkAndHardlink: symlink and hardlink
// entries are silently skipped (not an error, just not extracted).
// The key invariant is that no symlink ever lands on disk where a
// later regular-file write could follow it outside dest.
func TestSafeExtract_SkipsSymlinkAndHardlink(t *testing.T) {
	dest := t.TempDir()
	tgz := buildTarGz(t, []tarEntry{
		{Name: "skills-main/", Type: tar.TypeDir},
		{Name: "skills-main/link-to-passwd", Type: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "skills-main/hard-link", Type: tar.TypeLink, Linkname: "skills-main/regular"},
		{Name: "skills-main/regular", Type: tar.TypeReg, Body: []byte("ok")},
	})
	if err := safeExtractTarGz(bytes.NewReader(tgz), dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Regular files are extracted; links are not.
	if _, err := os.Lstat(filepath.Join(dest, "skills-main", "regular")); err != nil {
		t.Errorf("regular file should be extracted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "skills-main", "link-to-passwd")); err == nil {
		t.Errorf("symlink should not have been extracted")
	}
	if _, err := os.Lstat(filepath.Join(dest, "skills-main", "hard-link")); err == nil {
		t.Errorf("hardlink should not have been extracted")
	}
}

func TestSafeExtract_RejectsTooManyFiles(t *testing.T) {
	dest := t.TempDir()
	var entries []tarEntry
	for i := 0; i <= maxArchiveFiles+5; i++ {
		entries = append(entries, tarEntry{
			Name: filepath.Join("skills-main", "file-"+itoaInline(i)+".md"),
			Type: tar.TypeReg,
			Body: []byte("x"),
		})
	}
	tgz := buildTarGz(t, entries)
	if err := safeExtractTarGz(bytes.NewReader(tgz), dest); err == nil {
		t.Errorf("expected error for >maxArchiveFiles entries")
	}
}

// TestSafeExtract_RejectsOversizedExtract pumps a single entry
// whose uncompressed body exceeds maxExtractBytes. Extraction
// should fail loudly rather than silently truncating, so a hostile
// archive can't fill the disk.
func TestSafeExtract_RejectsOversizedExtract(t *testing.T) {
	dest := t.TempDir()
	huge := bytes.Repeat([]byte{'x'}, int(maxExtractBytes)+1024)
	tgz := buildTarGz(t, []tarEntry{
		{Name: "skills-main/", Type: tar.TypeDir},
		{Name: "skills-main/huge.md", Type: tar.TypeReg, Body: huge},
	})
	if err := safeExtractTarGz(bytes.NewReader(tgz), dest); err == nil {
		t.Errorf("expected error for oversized uncompressed payload")
	}
}

// TestInstall_NoAgentsDetected_DoesNotFetch confirms the order
// of checks: when no agent skills directory exists on the
// machine, the install command surfaces ErrNoAgentsDetected
// without ever touching the network. An offline user on a fresh
// machine should see the clear local error, not a fetch failure
// that hides the real cause.
func TestInstall_NoAgentsDetected_DoesNotFetch(t *testing.T) {
	disableAllAgentTargets(t) // every agent target explicitly disabled

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	defer srv.Close()
	withArchiveURL(t, srv.URL)

	_, err := Install(context.Background(), InstallOptions{CLIVersion: "v0.3.9"})
	if !errors.Is(err, ErrNoAgentsDetected) {
		t.Errorf("expected ErrNoAgentsDetected, got %v", err)
	}
	if hits != 0 {
		t.Errorf("expected 0 HTTP hits when no agent targets detected, got %d", hits)
	}
}

// TestInstall_SourceOverrideBypassesNetwork confirms the developer
// flow stays fast and offline: when --source is passed, no HTTP
// request fires.
func TestInstall_SourceOverrideBypassesNetwork(t *testing.T) {
	disableAllAgentTargets(t)
	t.Setenv(ClaudeSkillsDirEnv, filepath.Join(t.TempDir(), "claude-skills"))

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	defer srv.Close()
	withArchiveURL(t, srv.URL)

	srcRoot := filepath.Join(t.TempDir(), "src")
	writeSampleSkill(t, srcRoot)

	_, err := Install(context.Background(), InstallOptions{
		Source:     srcRoot,
		CLIVersion: "v0.3.9",
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if hits != 0 {
		t.Errorf("expected 0 HTTP hits with --source, got %d", hits)
	}
}

// --- Test helpers ---

// withArchiveURL points the package-level resolver at the supplied
// httptest URL for the duration of the test.
func withArchiveURL(t *testing.T, url string) {
	t.Helper()
	orig := archiveURLResolver
	archiveURLResolver = func() string { return url }
	t.Cleanup(func() { archiveURLResolver = orig })
}

// tarEntry describes one entry to bake into a test tarball.
type tarEntry struct {
	Name     string
	Type     byte
	Body     []byte
	Linkname string
}

// buildTarGz writes the supplied entries into a gzip-compressed
// tarball. Used to construct fixtures for happy-path and adverse
// extraction tests.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     0o644,
			Size:     int64(len(e.Body)),
			Typeflag: e.Type,
			Linkname: e.Linkname,
		}
		if e.Type == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar write header: %v", err)
		}
		if e.Type == tar.TypeReg && len(e.Body) > 0 {
			if _, err := io.Copy(tw, bytes.NewReader(e.Body)); err != nil {
				t.Fatalf("tar write body: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.Bytes()
}

// itoaInline avoids a strconv import for the file-count test.
func itoaInline(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
