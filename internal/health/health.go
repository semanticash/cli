// Package health composes read-only diagnostics for `semantica doctor`.
//
// Each check returns one or more Check results without mutating state.
// Run combines them into a Report, and render.go formats the result.
package health

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/launcher"
	"github.com/semanticash/cli/internal/util"
	"github.com/semanticash/cli/internal/version"
)

// SchemaVersion is the JSON schema version for the doctor report.
// Increment on breaking changes to the JSON shape.
const SchemaVersion = 1

// Status describes a single check's outcome.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Severity ordering for aggregation.
func severityRank(s Status) int {
	switch s {
	case StatusFail:
		return 2
	case StatusWarn:
		return 1
	default:
		return 0
	}
}

// Check is a single diagnostic result.
type Check struct {
	Category    string `json:"category"`
	ID          string `json:"id"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// Summary aggregates check counts by status.
type Summary struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

// Report is the full doctor result.
type Report struct {
	SchemaVersion int     `json:"schema_version"`
	Result        Status  `json:"result"`
	Summary       Summary `json:"summary"`
	Checks        []Check `json:"checks"`
}

// ExitCode maps the report's overall result to a process exit code.
// 0 = ok, 1 = warn, 2 = fail. CI gates can choose `< 2` to allow
// warns through or `== 0` to require fully clean.
func (r Report) ExitCode() int {
	return severityRank(r.Result)
}

// Options configure a doctor run. All fields are optional; zero
// values are safe defaults. Tests inject DoctorBinary and lookPath
// to avoid touching the real PATH.
type Options struct {
	// RepoPath is the working repository (default: cwd). Hook,
	// git-hook, and connect/auth checks are scoped to this repo.
	RepoPath string

	// DoctorBinary overrides `os.Executable()` for self-binary
	// matching. Used by tests.
	DoctorBinary string

	// LookPath overrides exec.LookPath. Used by tests.
	LookPath func(string) (string, error)

	// Registry is the explicit hook-provider registry used by the
	// hook-related checks (footguns, SQL inspection, provider hook
	// installation). Production callers must set this from
	// providers.NewHookRegistry(); the doctor command does so. A
	// nil Registry is treated as the empty set: hook-related
	// checks become no-ops and report no findings, which is
	// useful only for tests that exercise the non-hook paths
	// without wiring providers.
	Registry *hooks.Registry
}

// Run executes the health checks and returns a Report. Individual
// check errors are represented as Check results.
func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}

	// Walk PATH once so binary and hook checks use the same executable list.
	pathBins := findSemanticaOnPath(os.Getenv("PATH"))

	var checks []Check
	checks = append(checks, checkBinary(opts, pathBins)...)
	checks = append(checks, checkLauncher(ctx)...)
	checks = append(checks, checkHooks(ctx, opts, pathBins)...)
	checks = append(checks, checkState(ctx, opts)...)
	checks = append(checks, checkRecentEvents(ctx, opts)...)
	checks = append(checks, checkManifests(ctx, opts)...)
	checks = append(checks, checkProviderFootguns(ctx, opts)...)
	checks = append(checks, checkHookErrors(ctx)...)

	return assemble(checks), nil
}

// findSemanticaOnPath returns each distinct `semantica` executable on PATH.
// Symlinks are deduplicated while keeping the user-facing path in messages.
func findSemanticaOnPath(pathEnv string) []string {
	if pathEnv == "" {
		return nil
	}
	candidates := []string{"semantica"}
	if runtime.GOOS == "windows" {
		candidates = append(candidates, "semantica.exe")
	}

	var found []string
	seen := map[string]struct{}{}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		for _, name := range candidates {
			full := filepath.Join(dir, name)
			info, err := os.Stat(full)
			if err != nil || info.IsDir() {
				continue
			}
			if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
				continue
			}
			canonical, err := filepath.EvalSymlinks(full)
			if err != nil {
				canonical = full
			}
			if _, dup := seen[canonical]; dup {
				continue
			}
			seen[canonical] = struct{}{}
			found = append(found, full)
		}
	}
	return found
}

func assemble(checks []Check) Report {
	r := Report{
		SchemaVersion: SchemaVersion,
		Checks:        checks,
		Result:        StatusOK,
	}
	for _, c := range checks {
		switch c.Status {
		case StatusOK:
			r.Summary.OK++
		case StatusWarn:
			r.Summary.Warn++
		case StatusFail:
			r.Summary.Fail++
		}
		if severityRank(c.Status) > severityRank(r.Result) {
			r.Result = c.Status
		}
	}
	return r
}

// --- Binary checks ----------------------------------------------------------

func checkBinary(opts Options, pathBins []string) []Check {
	var checks []Check

	resolved, lookErr := opts.LookPath("semantica")
	if lookErr != nil || resolved == "" {
		checks = append(checks, Check{
			Category:    "binary",
			ID:          "path_resolves",
			Status:      StatusFail,
			Message:     "`semantica` not found on PATH",
			Remediation: "run `make install` or add Semantica to PATH",
		})
		checks = append(checks, Check{
			Category: "binary",
			ID:       "version",
			Status:   StatusOK,
			Message:  "build " + version.Short(),
		})
		return checks
	}

	checks = append(checks, Check{
		Category: "binary",
		ID:       "path_resolves",
		Status:   StatusOK,
		Message:  "resolved to " + resolved,
	})

	doctorBin := opts.DoctorBinary
	if doctorBin == "" {
		if exe, err := os.Executable(); err == nil {
			doctorBin = exe
		}
	}
	switch {
	case doctorBin == "":
		checks = append(checks, Check{
			Category: "binary",
			ID:       "self_match",
			Status:   StatusWarn,
			Message:  "could not determine doctor's own binary path; PATH ambiguity check skipped",
		})
	case sameBinary(resolved, doctorBin):
		checks = append(checks, Check{
			Category: "binary",
			ID:       "self_match",
			Status:   StatusOK,
			Message:  "PATH binary matches doctor's own build",
		})
	default:
		checks = append(checks, Check{
			Category:    "binary",
			ID:          "self_match",
			Status:      StatusFail,
			Message:     "PATH `semantica` (" + resolved + ") differs from doctor's binary (" + doctorBin + ")",
			Remediation: "remove the stale binary or re-run `make install`",
		})
	}

	checks = append(checks, Check{
		Category: "binary",
		ID:       "version",
		Status:   StatusOK,
		Message:  "build " + version.Short(),
	})

	if len(pathBins) > 1 {
		checks = append(checks, Check{
			Category:    "binary",
			ID:          "path_uniqueness",
			Status:      StatusWarn,
			Message:     "multiple `semantica` binaries on PATH: " + strings.Join(pathBins, ", "),
			Remediation: "remove the stale one(s); hooks that invoke bare `semantica` may pick up the wrong build",
		})
	}

	return checks
}

// sameBinary returns true when two paths refer to the same file.
func sameBinary(a, b string) bool {
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	if aErr == nil && bErr == nil && os.SameFile(ai, bi) {
		return true
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// --- Launcher checks --------------------------------------------------------

func checkLauncher(ctx context.Context) []Check {
	var checks []Check

	st, err := launcher.Status(ctx)
	if err != nil {
		checks = append(checks, Check{
			Category:    "launcher",
			ID:          "status",
			Status:      StatusFail,
			Message:     "launcher status query failed: " + err.Error(),
			Remediation: "run `semantica launcher status` for diagnostics",
		})
		return checks
	}

	switch st.ServiceState {
	case "loaded":
		checks = append(checks, Check{
			Category: "launcher",
			ID:       "status",
			Status:   StatusOK,
			Message:  "service running (" + st.UnitTarget + ")",
		})
	case "not loaded":
		if st.SettingsEnabled {
			checks = append(checks, Check{
				Category:    "launcher",
				ID:          "status",
				Status:      StatusFail,
				Message:     "launcher enabled in settings but not loaded by the OS daemon manager",
				Remediation: "run `semantica launcher enable`",
			})
		} else {
			checks = append(checks, Check{
				Category: "launcher",
				ID:       "status",
				Status:   StatusOK,
				Message:  "launcher not enabled (running CLI captures only)",
			})
		}
	case "unsupported":
		checks = append(checks, Check{
			Category: "launcher",
			ID:       "status",
			Status:   StatusOK,
			Message:  "launcher unsupported on this OS",
		})
	default:
		checks = append(checks, Check{
			Category: "launcher",
			ID:       "status",
			Status:   StatusWarn,
			Message:  "launcher state: " + st.ServiceState,
		})
	}

	if st.LogPath != "" {
		if _, err := os.Stat(st.LogPath); err == nil {
			checks = append(checks, Check{
				Category: "launcher",
				ID:       "worker_log",
				Status:   StatusOK,
				Message:  "worker log present at " + st.LogPath,
			})
		} else if os.IsNotExist(err) {
			if st.LoadedInDaemon {
				checks = append(checks, Check{
					Category: "launcher",
					ID:       "worker_log",
					Status:   StatusWarn,
					Message:  "worker log path " + st.LogPath + " does not exist yet",
				})
			} else {
				checks = append(checks, Check{
					Category: "launcher",
					ID:       "worker_log",
					Status:   StatusOK,
					Message:  "worker log absent (launcher not loaded)",
				})
			}
		} else {
			checks = append(checks, Check{
				Category:    "launcher",
				ID:          "worker_log",
				Status:      StatusWarn,
				Message:     "worker log " + st.LogPath + " unreadable: " + err.Error(),
				Remediation: "check filesystem permissions",
			})
		}
	}

	return checks
}

// --- Hook checks ------------------------------------------------------------

func checkHooks(ctx context.Context, opts Options, pathBins []string) []Check {
	var checks []Check

	repo := opts.RepoPath
	if repo == "" {
		checks = append(checks, Check{
			Category: "hooks",
			ID:       "scope",
			Status:   StatusOK,
			Message:  "no repo path supplied; hook checks skipped",
		})
		return checks
	}

	for _, p := range listRegistryProviders(opts) {
		if !p.AreHooksInstalled(ctx, repo) {
			continue
		}
		bin, err := p.HookBinary(ctx, repo)
		switch {
		case err != nil:
			checks = append(checks, Check{
				Category:    "hooks",
				ID:          "provider:" + p.Name(),
				Status:      StatusWarn,
				Message:     p.DisplayName() + ": installed, but hook binary could not be read: " + err.Error(),
				Remediation: "re-run `semantica enable` for this provider",
			})
		case bin == "":
			checks = append(checks, Check{
				Category:    "hooks",
				ID:          "provider:" + p.Name(),
				Status:      StatusWarn,
				Message:     p.DisplayName() + ": installed, but hook binary token is empty",
				Remediation: "re-run `semantica enable` for this provider",
			})
		case filepath.IsAbs(bin):
			doctorBin := opts.DoctorBinary
			if doctorBin == "" {
				if exe, err := os.Executable(); err == nil {
					doctorBin = exe
				}
			}
			if doctorBin != "" && !sameBinary(bin, doctorBin) {
				checks = append(checks, Check{
					Category:    "hooks",
					ID:          "provider:" + p.Name(),
					Status:      StatusFail,
					Message:     p.DisplayName() + ": hook points at " + bin + " but doctor runs as " + doctorBin,
					Remediation: "re-run `make install` or `semantica enable` for this provider",
				})
			} else {
				checks = append(checks, Check{
					Category: "hooks",
					ID:       "provider:" + p.Name(),
					Status:   StatusOK,
					Message:  p.DisplayName() + ": installed, hook binary " + bin,
				})
			}
		default:
			if len(pathBins) > 1 {
				checks = append(checks, Check{
					Category:    "hooks",
					ID:          "provider:" + p.Name(),
					Status:      StatusWarn,
					Message:     p.DisplayName() + ": installed with bare-name hook `" + bin + "`; PATH has multiple `semantica` binaries",
					Remediation: "remove stale binaries (see binary/path_uniqueness check above)",
				})
			} else {
				checks = append(checks, Check{
					Category: "hooks",
					ID:       "provider:" + p.Name(),
					Status:   StatusOK,
					Message:  p.DisplayName() + ": installed with bare-name hook `" + bin + "`",
				})
			}
		}
	}

	if len(checks) == 0 {
		checks = append(checks, Check{
			Category: "hooks",
			ID:       "providers",
			Status:   StatusOK,
			Message:  "no provider hooks installed in this repo",
		})
	}

	checks = append(checks, checkGitHooks(ctx, repo)...)
	return checks
}

func checkGitHooks(ctx context.Context, repoPath string) []Check {
	r, err := git.OpenRepo(repoPath)
	if err != nil {
		return []Check{{
			Category: "hooks",
			ID:       "git",
			Status:   StatusOK,
			Message:  "not a git repo; git-hook check skipped",
		}}
	}
	hooksDir, err := r.HooksDir(ctx)
	if err != nil {
		return []Check{{
			Category: "hooks",
			ID:       "git",
			Status:   StatusWarn,
			Message:  "could not resolve git hooks dir: " + err.Error(),
		}}
	}

	var checks []Check
	for _, name := range []string{"pre-commit", "post-commit"} {
		path := filepath.Join(hooksDir, name)
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			checks = append(checks, Check{
				Category: "hooks",
				ID:       "git:" + name,
				Status:   StatusOK,
				Message:  "git " + name + " hook not installed",
			})
		case err != nil:
			checks = append(checks, Check{
				Category: "hooks",
				ID:       "git:" + name,
				Status:   StatusWarn,
				Message:  "git " + name + " hook unreadable: " + err.Error(),
			})
		case strings.Contains(string(data), git.SemanticaHookMarker()):
			checks = append(checks, Check{
				Category: "hooks",
				ID:       "git:" + name,
				Status:   StatusOK,
				Message:  "git " + name + " hook installed by Semantica",
			})
		default:
			checks = append(checks, Check{
				Category: "hooks",
				ID:       "git:" + name,
				Status:   StatusOK,
				Message:  "git " + name + " hook present (not Semantica-owned)",
			})
		}
	}
	return checks
}

// --- State checks (capture, connect, auth) ----------------------------------

func checkState(ctx context.Context, opts Options) []Check {
	var checks []Check

	states, err := hooks.LoadActiveCaptureStates()
	switch {
	case err != nil:
		checks = append(checks, Check{
			Category:    "state",
			ID:          "capture_states",
			Status:      StatusWarn,
			Message:     "could not list capture states: " + err.Error(),
			Remediation: "check `~/.semantica/capture/` exists and is readable",
		})
	case len(states) == 0:
		checks = append(checks, Check{
			Category: "state",
			ID:       "capture_states",
			Status:   StatusOK,
			Message:  "no active capture sessions (idle is normal)",
		})
	default:
		perProvider := map[string]int{}
		for _, s := range states {
			perProvider[s.Provider]++
		}
		var parts []string
		for prov, n := range perProvider {
			parts = append(parts, prov+":"+itoa(n))
		}
		checks = append(checks, Check{
			Category: "state",
			ID:       "capture_states",
			Status:   StatusOK,
			Message:  "active capture sessions: " + strings.Join(parts, ", "),
		})
	}

	checks = append(checks, checkConnectAndAuth(opts)...)
	return checks
}

func checkConnectAndAuth(opts Options) []Check {
	var checks []Check
	connected := false

	if opts.RepoPath == "" {
		checks = append(checks, Check{
			Category: "state",
			ID:       "connect",
			Status:   StatusOK,
			Message:  "no repo path supplied; connect check skipped",
		})
	} else {
		semDir := filepath.Join(opts.RepoPath, ".semantica")
		settings, err := util.ReadSettings(semDir)
		switch {
		case err != nil:
			checks = append(checks, Check{
				Category:    "state",
				ID:          "connect",
				Status:      StatusWarn,
				Message:     "settings.json unreadable: " + err.Error(),
				Remediation: "your CLI may be older than the settings file; upgrade",
			})
		case settings.ConnectedRepoID == "":
			checks = append(checks, Check{
				Category: "state",
				ID:       "connect",
				Status:   StatusOK,
				Message:  "workspace not connected (local capture only)",
			})
		default:
			connected = true
			checks = append(checks, Check{
				Category: "state",
				ID:       "connect",
				Status:   StatusOK,
				Message:  "workspace connected (repo " + settings.ConnectedRepoID + ")",
			})
		}
	}

	checks = append(checks, checkAuthFor(connected))
	return checks
}

// checkAuthFor evaluates auth state with the same auth surface as `status`.
func checkAuthFor(connected bool) Check {
	return classifyAuth(auth.GetAuthState(), connected)
}

// classifyAuth maps auth state and repo connection state into a Check.
func classifyAuth(state auth.AuthState, connected bool) Check {
	switch {
	case state.StorageError != "":
		return Check{
			Category:    "state",
			ID:          "auth",
			Status:      StatusWarn,
			Message:     "credential storage error: " + state.StorageError,
			Remediation: "check keyring permissions, then re-run `semantica auth login`",
		}
	case state.Authenticated && state.Source == "api_key":
		return Check{
			Category: "state",
			ID:       "auth",
			Status:   StatusOK,
			Message:  "authenticated via SEMANTICA_API_KEY",
		}
	case state.Authenticated:
		msg := "authenticated"
		if state.Email != "" {
			msg += " as " + state.Email
		}
		return Check{
			Category: "state",
			ID:       "auth",
			Status:   StatusOK,
			Message:  msg,
		}
	case connected:
		return Check{
			Category:    "state",
			ID:          "auth",
			Status:      StatusWarn,
			Message:     "workspace connected but not authenticated",
			Remediation: "run `semantica auth login`",
		}
	default:
		return Check{
			Category: "state",
			ID:       "auth",
			Status:   StatusOK,
			Message:  "not authenticated (local-only mode)",
		}
	}
}

// itoa is a tiny stdlib-free integer formatter to avoid importing
// strconv in this otherwise-string-heavy file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// listRegistryProviders returns the hook providers to iterate over
// for hook-related health checks. opts.Registry is the explicit
// hook-provider registry the doctor command wires from
// internal/providers.NewHookRegistry(). Callers that pass a nil
// Registry get an empty slice and the hook-related checks become
// no-ops; production wiring always sets Registry, so this only
// affects tests that intentionally exercise the no-registry path.
func listRegistryProviders(opts Options) []hooks.HookProvider {
	if opts.Registry == nil {
		return nil
	}
	return opts.Registry.List()
}
