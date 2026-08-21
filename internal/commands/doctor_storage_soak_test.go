package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/version"
)

func gitInitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// enableRepo initializes capture storage without creating the coordination lock.
func enableRepo(t *testing.T, repoPath string) (*toolsnap.Store, *toolsnap.Registry) {
	t.Helper()
	ctx := context.Background()
	semDir := filepath.Join(repoPath, ".semantica")
	rc, err := toolsnap.ResolveRepoContext(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := toolsnap.OpenStore(ctx, rc, semDir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := toolsnap.OpenRegistry(semDir)
	if err != nil {
		t.Fatal(err)
	}
	return store, reg
}

// initCaptureStorage initializes capture storage and its coordination lock.
func initCaptureStorage(t *testing.T, repoPath string) {
	t.Helper()
	store, reg := enableRepo(t, repoPath)
	if _, err := store.Maintain(context.Background(), reg, 0); err != nil {
		t.Fatal(err)
	}
}

// semanticaTreeFingerprint records files, directories, and symlinks under
// .semantica so tests can detect any mutation.
func semanticaTreeFingerprint(t *testing.T, repoPath string) map[string]string {
	t.Helper()
	root := filepath.Join(repoPath, ".semantica")
	fp := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info() // Lstat semantics: does not follow symlinks
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		switch {
		case d.IsDir():
			fp[rel] = fmt.Sprintf("dir:%o", info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fp[rel] = "symlink:" + target
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fp[rel] = fmt.Sprintf("file:%o:%d:%d:%x", info.Mode().Perm(), info.Size(), info.ModTime().UnixNano(), sha256.Sum256(data))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestAppendStorageSoakLog(t *testing.T) {
	repo := t.TempDir()
	if err := appendStorageSoakLog(repo, []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := appendStorageSoakLog(repo, []byte(`{"n":2}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(storageSoakLogPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 || lines[0] != `{"n":1}` || lines[1] != `{"n":2}` {
		t.Fatalf("series = %q", lines)
	}
}

func TestInspectRepoStorageAbsentIsClearError(t *testing.T) {
	repo := gitInitRepo(t) // no capture storage initialized
	_, err := inspectRepoStorage(context.Background(), repo, time.Now())
	if !errors.Is(err, toolsnap.ErrCaptureStorageAbsent) {
		t.Fatalf("err = %v, want ErrCaptureStorageAbsent", err)
	}
}

// Inspection requires the coordination lock created by capture or maintenance.
func TestInspectRepoStorageNoLockIsClearError(t *testing.T) {
	repo := gitInitRepo(t)
	enableRepo(t, repo) // store + registry, but no capture/maintenance
	_, err := inspectRepoStorage(context.Background(), repo, time.Now())
	if !errors.Is(err, toolsnap.ErrCaptureStorageAbsent) {
		t.Fatalf("err = %v, want ErrCaptureStorageAbsent", err)
	}
}

func TestInspectRepoStorageSmoke(t *testing.T) {
	repo := gitInitRepo(t)
	initCaptureStorage(t, repo)
	insp, err := inspectRepoStorage(context.Background(), repo, time.Now())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if insp.Deferred {
		t.Fatalf("unexpected deferral: %s", insp.DeferReason)
	}
	if insp.Now.IsZero() || insp.RefCutoff.IsZero() {
		t.Error("pinned timestamps missing")
	}
}

// The command prints one record and optionally logs the same bytes.
func TestStorageSoakCommandLogsSeries(t *testing.T) {
	repo := gitInitRepo(t)
	initCaptureStorage(t, repo)
	cmd := newDoctorStorageSoakCmd(&RootOptions{RepoPath: repo})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--log"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	printed := strings.TrimRight(out.String(), "\n")
	var rec storageSoakRecord
	if err := json.Unmarshal([]byte(printed), &rec); err != nil {
		t.Fatalf("stdout is not a valid record: %v", err)
	}
	if rec.SchemaVersion != storageSoakSchemaVersion {
		t.Errorf("schema_version = %d, want %d", rec.SchemaVersion, storageSoakSchemaVersion)
	}
	if rec.CLIVersion != version.Version {
		t.Errorf("cli_version = %q, want %q", rec.CLIVersion, version.Version)
	}

	data, err := os.ReadFile(storageSoakLogPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if logged := strings.TrimRight(string(data), "\n"); logged != printed {
		t.Errorf("logged line != printed line\n log: %s\nprint: %s", logged, printed)
	}
}

// The record identifies the build revision that produced it.
func TestStorageSoakCommandRecordsCommit(t *testing.T) {
	repo := gitInitRepo(t)
	initCaptureStorage(t, repo)
	orig := version.Commit
	version.Commit = "abc1234def"
	defer func() { version.Commit = orig }()

	cmd := newDoctorStorageSoakCmd(&RootOptions{RepoPath: repo})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var rec storageSoakRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.CLICommit != "abc1234def" {
		t.Errorf("cli_commit = %q, want %q", rec.CLICommit, "abc1234def")
	}
}

// Without --log the command must not modify any file under .semantica.
func TestStorageSoakCommandIsReadOnly(t *testing.T) {
	repo := gitInitRepo(t)
	initCaptureStorage(t, repo)
	before := semanticaTreeFingerprint(t, repo)

	cmd := newDoctorStorageSoakCmd(&RootOptions{RepoPath: repo})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	after := semanticaTreeFingerprint(t, repo)
	if !reflect.DeepEqual(before, after) {
		t.Errorf(".semantica tree changed by a read-only run\nbefore: %v\nafter:  %v", before, after)
	}
}
