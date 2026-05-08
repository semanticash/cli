package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ClaudeSkillsDirEnv lets tests override the Claude Code skills
// root without redirecting the entire user home. Set it to a temp
// path to exercise install / uninstall flows hermetically.
const ClaudeSkillsDirEnv = "SEMANTICA_CLAUDE_SKILLS_DIR"

// SkillFileName is the fixed name Anthropic Agent Skills loaders
// expect inside each skill subdirectory.
const SkillFileName = "SKILL.md"

// SemanticaSkillNamePrefix is the prefix every Semantica-owned
// skill identifier carries. Both the source directory name and the
// destination directory name must start with it. The prefix is the
// scoping boundary for uninstall: directories without it are
// considered third-party or user-authored and are never touched,
// regardless of the --force flag.
const SemanticaSkillNamePrefix = "semantica-"

// safeSkillName matches the on-disk identifier shape: a leading
// letter, lowercase alnum and hyphens after, length capped. Anchored
// so any path separator, traversal, or whitespace fails before any
// write. Combined with SemanticaSkillNamePrefix, it pins the two
// conditions we want at every install/uninstall boundary: the
// directory name is filesystem-safe and unambiguously ours.
var safeSkillName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// ErrFileEdited indicates an installed SKILL.md is Semantica-managed
// but has been modified since install. Install and uninstall refuse
// to act on these files unless --force is set.
var ErrFileEdited = errors.New("installed SKILL.md has been edited since install")

// ErrFileUnmanaged indicates a SKILL.md exists at the destination
// but is missing the Semantica ownership marker. Install refuses to
// overwrite it unless --force is set. Uninstall preserves it under
// all flags.
var ErrFileUnmanaged = errors.New("destination SKILL.md is not Semantica-managed")

// ErrSourceMissing indicates the install source directory does not
// exist or is unreadable.
var ErrSourceMissing = errors.New("source directory does not exist")

// ErrSourceNoSkills indicates the install source directory exists
// but contains no `<skill-name>/SKILL.md` entries.
var ErrSourceNoSkills = errors.New("source directory contains no skills")

// ErrUnsafeSkillName indicates a skill's directory name or
// frontmatter name does not match the safe-name pattern. The
// install refuses rather than risking writes outside the skills
// directory tree.
var ErrUnsafeSkillName = errors.New("skill name is not safe for filesystem use")

// ErrSkillNameNotPrefixed indicates a skill's identifier does not
// carry the SemanticaSkillNamePrefix. The CLI refuses to install
// such a file because the prefix is also the uninstall scoping
// boundary: a non-prefixed install would either leak past
// uninstall or risk colliding with an unrelated agent-side skill.
var ErrSkillNameNotPrefixed = errors.New("skill name must start with " + SemanticaSkillNamePrefix)

// ErrSkillNameMismatch indicates a skill's source directory name
// does not match its frontmatter `name` field. AUTHORING.md
// requires they match.
var ErrSkillNameMismatch = errors.New("skill directory name does not match frontmatter name")

// ActionKind labels what Install or Uninstall did to a single
// skill file. Used by the report so the command layer can render
// a sensible per-skill summary.
type ActionKind string

const (
	ActionInstalled ActionKind = "installed"
	ActionUpdated   ActionKind = "updated"
	ActionRemoved   ActionKind = "removed"
	ActionSkipped   ActionKind = "skipped"
	ActionForced    ActionKind = "forced"
)

// SkillAction is one row in an Install or Uninstall report.
type SkillAction struct {
	Skill  string
	Path   string
	Action ActionKind
	Reason string // populated for ActionSkipped / ActionForced
}

// Report is returned from Install and Uninstall. The command layer
// renders the rows; this package does not print anything itself.
type Report struct {
	Actions []SkillAction
}

// InstallOptions controls Install. Source is the directory laid
// out as `<source>/<skill-name>/SKILL.md` (the layout the skills
// repo uses). CLIVersion is stamped into each installed file.
type InstallOptions struct {
	Source     string
	CLIVersion string
	Force      bool
}

// Install walks the source tree, stamps each SKILL.md, and writes
// the result to the Claude Code user-global skills directory. The
// function is idempotent for files that pass Verify against the
// existing destination: re-running with the same CLI version is a
// no-op write of identical bytes; re-running after a CLI version
// bump rewrites with the new version stamped in. Files that have
// been edited since install, or that exist at the destination
// without the Semantica ownership marker, are refused unless
// Force is set.
func Install(opts InstallOptions) (*Report, error) {
	if opts.Source == "" {
		return nil, ErrSourceMissing
	}
	if opts.CLIVersion == "" {
		return nil, ErrCLIVersionEmpty
	}
	st, err := os.Stat(opts.Source)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrSourceMissing, opts.Source)
	}

	dstRoot, err := claudeSkillsDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(opts.Source)
	if err != nil {
		return nil, fmt.Errorf("read source dir: %w", err)
	}

	var rep Report
	var found int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()
		srcPath := filepath.Join(opts.Source, dirName, SkillFileName)
		if _, err := os.Stat(srcPath); err != nil {
			continue
		}
		found++

		action, actErr := installOne(opts, dirName, srcPath, dstRoot)
		if actErr != nil {
			return nil, fmt.Errorf("install %s: %w", dirName, actErr)
		}
		rep.Actions = append(rep.Actions, action)
	}

	if found == 0 {
		return nil, fmt.Errorf("%w: %s", ErrSourceNoSkills, opts.Source)
	}
	sortReport(&rep)
	return &rep, nil
}

// installOne performs the integrity checks and write for a single
// skill. The directory name must match the frontmatter name;
// destination paths are derived from the validated frontmatter
// name only, so an attacker-controlled directory name cannot push
// content outside the skills root.
func installOne(opts InstallOptions, dirName, srcPath, dstRoot string) (SkillAction, error) {
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return SkillAction{}, fmt.Errorf("read source: %w", err)
	}

	stamped, _, err := Stamp(src, opts.CLIVersion)
	if err != nil {
		return SkillAction{}, fmt.Errorf("stamp source: %w", err)
	}

	name, ok := readFrontmatterValue(stamped, "name")
	if !ok || !safeSkillName.MatchString(name) {
		return SkillAction{}, fmt.Errorf("%w: %q", ErrUnsafeSkillName, name)
	}
	if !safeSkillName.MatchString(dirName) {
		return SkillAction{}, fmt.Errorf("%w: %q", ErrUnsafeSkillName, dirName)
	}
	if !strings.HasPrefix(name, SemanticaSkillNamePrefix) {
		return SkillAction{}, fmt.Errorf("%w (got %q)", ErrSkillNameNotPrefixed, name)
	}
	if name != dirName {
		return SkillAction{}, fmt.Errorf("%w: dir=%q frontmatter=%q",
			ErrSkillNameMismatch, dirName, name)
	}

	dstDir := filepath.Join(dstRoot, name)
	dstPath := filepath.Join(dstDir, SkillFileName)

	existing, statErr := os.ReadFile(dstPath)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		// Fresh install.
	case statErr != nil:
		return SkillAction{}, fmt.Errorf("read existing destination: %w", statErr)
	default:
		ok, vErr := Verify(existing)
		switch {
		case errors.Is(vErr, ErrManagedMarkerMissing):
			if !opts.Force {
				return SkillAction{
					Skill:  name,
					Path:   dstPath,
					Action: ActionSkipped,
					Reason: "destination SKILL.md is not Semantica-managed; use --force to overwrite",
				}, nil
			}
		case vErr != nil:
			return SkillAction{}, fmt.Errorf("verify existing destination: %w", vErr)
		case !ok:
			if !opts.Force {
				return SkillAction{
					Skill:  name,
					Path:   dstPath,
					Action: ActionSkipped,
					Reason: "installed SKILL.md has been edited since install; use --force to overwrite",
				}, nil
			}
		}
	}

	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return SkillAction{}, fmt.Errorf("create destination dir: %w", err)
	}
	if err := os.WriteFile(dstPath, stamped, 0o644); err != nil {
		return SkillAction{}, fmt.Errorf("write destination: %w", err)
	}

	if existing == nil {
		return SkillAction{Skill: name, Path: dstPath, Action: ActionInstalled}, nil
	}
	return SkillAction{Skill: name, Path: dstPath, Action: ActionUpdated}, nil
}

// Uninstall scans the Claude Code user-global skills directory and
// removes Semantica-installed SKILL.md files. Discovery is scoped
// to directories whose name starts with SemanticaSkillNamePrefix.
// third-party or user-authored skills (e.g. `~/.claude/skills/review/`)
// are out of scope and never touched, regardless of the force flag.
//
// Within scope:
//   - hash matches stored value: removed (ActionRemoved).
//   - hash mismatch (we wrote it, user later edited it): preserved
//     unless force is set, in which case removed (ActionForced).
//   - missing managed marker: preserved under all flags. The marker
//     is the only positive signal that the file is ours; if it is
//     gone we treat the file as user-authored content that happens
//     to live under our prefix.
//
// Skill subdirectories are removed best-effort once their SKILL.md
// is gone, but only when the directory ends up empty so user-added
// sibling files are preserved.
func Uninstall(force bool) (*Report, error) {
	dstRoot, err := claudeSkillsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dstRoot)
	if errors.Is(err, os.ErrNotExist) {
		return &Report{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read skills dir: %w", err)
	}

	var rep Report
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Scoping guard: a directory is a Semantica candidate only
		// if its name starts with our prefix AND fits the safe-name
		// pattern. Anything else is third-party content; skipping
		// it silently is correct because uninstall is scoped to our
		// own files.
		if !strings.HasPrefix(name, SemanticaSkillNamePrefix) {
			continue
		}
		if !safeSkillName.MatchString(name) {
			continue
		}
		path := filepath.Join(dstRoot, name, SkillFileName)
		body, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		ok, vErr := Verify(body)
		switch {
		case errors.Is(vErr, ErrManagedMarkerMissing):
			// Prefix matches but no marker: refuse to delete under
			// any flag. The user could have authored a SKILL.md
			// here themselves; --force should never let us delete
			// content we never marked as ours.
			rep.Actions = append(rep.Actions, SkillAction{
				Skill: name, Path: path, Action: ActionSkipped,
				Reason: "not Semantica-managed (marker missing); refusing to remove",
			})
			continue
		case vErr != nil:
			return nil, fmt.Errorf("verify %s: %w", path, vErr)
		case !ok:
			if !force {
				rep.Actions = append(rep.Actions, SkillAction{
					Skill: name, Path: path, Action: ActionSkipped,
					Reason: "edited since install; use --force to remove",
				})
				continue
			}
		}

		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove %s: %w", path, err)
		}
		// Best-effort: drop the skill subdirectory if it is now
		// empty. Failing here is non-fatal because the SKILL.md is
		// already gone.
		_ = os.Remove(filepath.Join(dstRoot, name))

		action := SkillAction{Skill: name, Path: path, Action: ActionRemoved}
		if !ok {
			// Hash-mismatch removal is the only --force path now,
			// since the marker-missing case never reaches here.
			action.Action = ActionForced
			action.Reason = "removed edited file under --force"
		}
		rep.Actions = append(rep.Actions, action)
	}

	sortReport(&rep)
	return &rep, nil
}

// claudeSkillsDir resolves to "$HOME/.claude/skills" with an env
// override for tests. Claude Code is the only loader supported in
// this release; other agents can be added after their loader paths
// are verified.
func claudeSkillsDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv(ClaudeSkillsDirEnv)); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// sortReport orders actions by skill name so the command layer's
// output is deterministic regardless of directory iteration order.
func sortReport(r *Report) {
	sort.Slice(r.Actions, func(i, j int) bool { return r.Actions[i].Skill < r.Actions[j].Skill })
}
