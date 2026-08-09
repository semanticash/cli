package toolsnap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// storeDirName is the bare snapshot store location under .semantica.
const storeDirName = "tool-snapshots.git"

// Store is the isolated bare Git object store holding ephemeral
// snapshot trees. It reads the user repository's objects through a
// read-only alternate but never writes to them.
type Store struct {
	// Dir is the bare store's git directory.
	Dir  string
	repo RepoContext

	// MaxCandidatePaths and MaxBytesRead bound one snapshot; zero
	// values apply the package defaults.
	MaxCandidatePaths int
	MaxBytesRead      int64
}

func (s *Store) maxPaths() int {
	if s.MaxCandidatePaths > 0 {
		return s.MaxCandidatePaths
	}
	return DefaultMaxCandidatePaths
}

func (s *Store) maxBytes() int64 {
	if s.MaxBytesRead > 0 {
		return s.MaxBytesRead
	}
	return DefaultMaxBytesRead
}

// ErrStoreIncompatible reports an object-format mismatch between the
// snapshot store and repository.
var ErrStoreIncompatible = errors.New("toolsnap: snapshot store incompatible with repository")

// OpenStore opens or initializes the isolated snapshot store for the
// repository, wiring the repository's common object directory as a
// read-only alternate. semDir is the repository's .semantica directory.
func OpenStore(ctx context.Context, rc RepoContext, semDir string) (*Store, error) {
	dir := filepath.Join(semDir, storeDirName)
	s := &Store{Dir: dir, repo: rc}

	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("toolsnap: probe store: %w", err)
		}
		if err := s.initialize(ctx); err != nil {
			return nil, err
		}
	}

	if err := s.verifyCompatible(ctx); err != nil {
		return nil, err
	}
	if err := s.ensureAlternate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.Dir), 0o755); err != nil {
		return fmt.Errorf("toolsnap: create store parent: %w", err)
	}
	// The store must share the repository's object format or tree
	// entries referencing repository blobs would be malformed.
	_, err := gitOutput(ctx, filepath.Dir(s.Dir),
		"init", "--bare", "--object-format="+s.repo.ObjectFormat, s.Dir)
	if err != nil {
		return fmt.Errorf("toolsnap: init store: %w", err)
	}
	// Semantica schedules maintenance while no snapshot capture is active.
	_, err = s.git(ctx, "config", "gc.auto", "0")
	if err != nil {
		return fmt.Errorf("toolsnap: disable auto gc: %w", err)
	}
	return nil
}

func (s *Store) verifyCompatible(ctx context.Context) error {
	format, err := s.git(ctx, "rev-parse", "--show-object-format")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStoreIncompatible, err)
	}
	if strings.TrimSpace(format) != s.repo.ObjectFormat {
		return fmt.Errorf("%w: store %q, repository %q",
			ErrStoreIncompatible, strings.TrimSpace(format), s.repo.ObjectFormat)
	}
	return nil
}

// ensureAlternate points the store at the repository's object database
// read-only. The alternates file is idempotently rewritten: exactly one
// entry, the repository common objects directory.
func (s *Store) ensureAlternate() error {
	objectsInfo := filepath.Join(s.Dir, "objects", "info")
	if err := os.MkdirAll(objectsInfo, 0o755); err != nil {
		return fmt.Errorf("toolsnap: create objects/info: %w", err)
	}
	target := filepath.Join(s.repo.CommonDir, "objects")
	path := filepath.Join(objectsInfo, "alternates")
	want := target + "\n"
	if cur, err := os.ReadFile(path); err == nil && string(cur) == want {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(want), 0o644); err != nil {
		return fmt.Errorf("toolsnap: write alternates: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("toolsnap: install alternates: %w", err)
	}
	return nil
}

// git runs a git command against the bare store, isolated from the
// user's environment discovery.
func (s *Store) git(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--git-dir", s.Dir}, args...)
	return gitOutput(ctx, filepath.Dir(s.Dir), full...)
}
