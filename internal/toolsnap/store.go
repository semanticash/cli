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

// ErrCaptureStorageAbsent reports that capture storage has not been
// initialized, so there is nothing to inspect.
var ErrCaptureStorageAbsent = errors.New("toolsnap: capture storage not initialized")

// OpenStoreForInspection opens an existing snapshot store without modifying
// it. It rejects missing, non-directory, and symlinked stores, then validates
// the config, object format, and alternate object database.
func OpenStoreForInspection(ctx context.Context, rc RepoContext, semDir string) (*Store, error) {
	dir := filepath.Join(semDir, storeDirName)
	s := &Store{Dir: dir, repo: rc}
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCaptureStorageAbsent
		}
		return nil, fmt.Errorf("toolsnap: probe store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("toolsnap: store directory is a symlink")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("toolsnap: store path is not a directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCaptureStorageAbsent
		}
		return nil, fmt.Errorf("toolsnap: probe store: %w", err)
	}
	if err := s.verifyInspectable(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// verifyInspectable validates the store config, object format, and alternate
// without repairing them.
func (s *Store) verifyInspectable(ctx context.Context) error {
	if err := s.verifyCompatible(ctx); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, "config"))
	if err != nil {
		return fmt.Errorf("toolsnap: read store config: %w", err)
	}
	// Accept only the configuration shape created for snapshot stores.
	format, ok := parseStoreObjectFormat(string(raw))
	if !ok || format != s.repo.ObjectFormat {
		return fmt.Errorf("%w: unexpected store config", ErrStoreIncompatible)
	}
	alt, err := os.ReadFile(filepath.Join(s.Dir, "objects", "info", "alternates"))
	if err != nil {
		return fmt.Errorf("toolsnap: read store alternates: %w", err)
	}
	want := filepath.Join(s.repo.CommonDir, "objects") + "\n"
	if string(alt) != want {
		return fmt.Errorf("%w: store alternate %q, want %q",
			ErrStoreIncompatible, strings.TrimSpace(string(alt)), strings.TrimSpace(want))
	}
	return nil
}

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

	// Fast path: the store config is Semantica-owned and tiny. When a
	// direct read shows the expected object format and no trace of
	// fetch-capable configuration, the git-based verification and
	// sanitization spawns are skipped; anything suspicious takes the
	// authoritative slow path. This keeps hook-path store opening free
	// of git invocations.
	if !s.configFastPathClean() {
		if err := s.verifyCompatible(ctx); err != nil {
			return nil, err
		}
		if err := s.sanitizeConfig(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.ensureAlternate(); err != nil {
		return nil, err
	}
	return s, nil
}

// configFastPathClean reports whether the store config file can be
// accepted without spawning git. Two independent checks must pass:
// an over-broad token scan (a false positive only costs the slow
// path) and a strict section/key parse of the object format, because
// granting the fast path from substring matching would let a comment
// mentioning sha256 misclassify the store.
func (s *Store) configFastPathClean() bool {
	raw, err := os.ReadFile(filepath.Join(s.Dir, "config"))
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(raw))
	for _, token := range []string{"remote", "include", "partialclone", "promisor"} {
		if strings.Contains(lower, token) {
			return false
		}
	}
	format, ok := parseStoreObjectFormat(string(raw))
	return ok && format == s.repo.ObjectFormat
}

// allowedCoreKeys are the core keys git init writes for a bare store
// across supported platforms. Anything else is not a configuration
// Semantica generates and forces the slow path.
var allowedCoreKeys = map[string]bool{
	"repositoryformatversion": true,
	"filemode":                true,
	"bare":                    true,
	"logallrefupdates":        true,
	"ignorecase":              true,
	"precomposeunicode":       true,
	"symlinks":                true,
}

// parseStoreObjectFormat accepts only the configuration shape a
// Semantica-initialized store has: the generated minimum of
// repositoryformatversion, bare = true, and gc.auto = 0 must all be
// present, other whitelisted core keys may appear, and
// extensions.objectformat requires repository format version 1. Any
// unknown section, key, subsection, deviating value, or malformed
// line returns not-ok so the authoritative git-based path decides.
func parseStoreObjectFormat(raw string) (string, bool) {
	section := ""
	format := "sha1"
	version := ""
	sawBare := false
	sawGcAutoZero := false
	sawExtensions := false
	for _, line := range strings.Split(raw, "\n") {
		// Strip comments; neither section names nor the values this
		// parser accepts may legally contain # or ;.
		if idx := strings.IndexAny(line, "#;"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return "", false
			}
			section = strings.ToLower(strings.TrimSpace(strings.Trim(line, "[]")))
			// Subsections never appear in a store Semantica created.
			if strings.ContainsAny(section, " \t\"") {
				return "", false
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", false
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.Trim(strings.TrimSpace(value), `"`))
		switch section {
		case "core":
			if !allowedCoreKeys[key] {
				return "", false
			}
			switch key {
			case "bare":
				if value != "true" {
					return "", false
				}
				sawBare = true
			case "repositoryformatversion":
				if value != "0" && value != "1" {
					return "", false
				}
				version = value
			}
		case "extensions":
			if key != "objectformat" || (value != "sha1" && value != "sha256") {
				return "", false
			}
			sawExtensions = true
			format = value
		case "gc":
			if key != "auto" || value != "0" {
				return "", false
			}
			sawGcAutoZero = true
		default:
			return "", false
		}
	}
	if !sawBare || !sawGcAutoZero || version == "" {
		return "", false
	}
	// Extensions require repository format version 1.
	if sawExtensions && version != "1" {
		return "", false
	}
	return format, true
}

// sanitizeConfig removes fetch-capable settings from the Semantica-owned
// store. Snapshot stores never need remotes, partial-clone settings, or
// external config includes.
func (s *Store) sanitizeConfig(ctx context.Context) error {
	list, err := s.git(ctx, "config", "--local", "--list", "--name-only")
	if err != nil {
		return fmt.Errorf("toolsnap: read store config: %w", err)
	}
	sections := map[string]bool{}
	unset := []string{}
	for _, name := range strings.Split(strings.TrimSpace(list), "\n") {
		lower := strings.ToLower(strings.TrimSpace(name))
		switch {
		case strings.HasPrefix(lower, "remote."):
			if idx := strings.LastIndexByte(name, '.'); idx > 0 {
				sections[name[:idx]] = true
			}
		// Object reads expand include.path and includeIf.*.path. Remove
		// includes by full key because conditional subsection names are
		// not valid --remove-section arguments.
		case strings.HasPrefix(lower, "include."),
			strings.HasPrefix(lower, "includeif."),
			lower == "extensions.partialclone":
			unset = append(unset, name)
		}
	}
	for section := range sections {
		if _, err := s.git(ctx, "config", "--remove-section", section); err != nil {
			return fmt.Errorf("toolsnap: remove store config section %s: %w", section, err)
		}
	}
	for _, name := range unset {
		if _, err := s.git(ctx, "config", "--local", "--unset-all", name); err != nil {
			return fmt.Errorf("toolsnap: unset store config %s: %w", name, err)
		}
	}

	// Verify that the store cannot resolve a transport source.
	list, err = s.git(ctx, "config", "--local", "--list", "--name-only")
	if err != nil {
		return fmt.Errorf("toolsnap: re-read store config: %w", err)
	}
	for _, name := range strings.Split(strings.ToLower(strings.TrimSpace(list)), "\n") {
		name = strings.TrimSpace(name)
		if strings.HasPrefix(name, "remote.") ||
			strings.HasPrefix(name, "include.") ||
			strings.HasPrefix(name, "includeif.") ||
			name == "extensions.partialclone" {
			return fmt.Errorf("%w: fetch-enabling config %q persists after sanitization", ErrStoreIncompatible, name)
		}
	}
	return nil
}

func (s *Store) initialize(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.Dir), 0o755); err != nil {
		return fmt.Errorf("toolsnap: create store parent: %w", err)
	}
	// Matching object formats let snapshot trees reference repository blobs.
	// Config isolation also prevents inherited templates from modifying the store.
	_, err := gitOutputEnv(ctx, filepath.Dir(s.Dir), storeGitEnv(nil),
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
	return gitOutputEnv(ctx, filepath.Dir(s.Dir), storeGitEnv(nil), full...)
}
