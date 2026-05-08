// Package explain implements the backing engine for the
// `semantica skills explain` command. Explain currently returns
// git-only output after ref resolution; local and remote provenance
// can be added without changing the JSON contract.
//
// The contract: callers always get a structured Output back when
// the engine could decide what to say. Errors from this package are
// reserved for genuine command-runtime failures (CLI bug, missing
// git binary, etc.), so SKILL.md bodies can rely on a single rule:
// non-zero exit means "something broke, print stderr"; zero exit
// means "parse the JSON and render per the mode field".
package explain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/redact"
)

// MaxDiffBytes caps the redacted diff_excerpt length so the JSON
// payload stays small. Matches the bound auto-playbook uses for
// similar bounded views.
//
// Order matters: the bound is applied AFTER redaction, never
// before. If we truncated the raw diff first, a secret that
// straddled the boundary would reach the redactor as a partial
// fragment that the gitleaks regexes might miss, leaking the
// pre-cutoff portion into the agent's response. Redacting the
// whole diff first guarantees every secret token is matched
// against complete content; only redaction-safe bytes (literal
// markdown plus [REDACTED] tokens) are then truncated.
const MaxDiffBytes = 12_000

// maxDiffBytes is the effective bound used by gitOnly. Production
// code initializes it from MaxDiffBytes; tests override it to keep
// fixtures small while still exercising truncation paths. Restore
// in t.Cleanup.
var maxDiffBytes = MaxDiffBytes

// truncatedMarker is appended to a diff that the engine truncated.
// SKILL.md authors and tests rely on the literal text.
const truncatedMarker = "\n... (truncated)"

// Mode is the top-level dispatcher in the JSON contract. The skill
// body switches on it to render the right shape.
type Mode string

const (
	ModeProvenance Mode = "provenance"
	ModeGitOnly    Mode = "git-only"
	ModeBlocked    Mode = "blocked"
	ModeNotFound   Mode = "not-found"
)

// FallbackReason explains why the engine produced a git-only
// summary instead of provenance. The git-only path emits
// remote_not_attempted; API-backed modes can use the other values.
type FallbackReason string

const (
	FallbackNotInRemote        FallbackReason = "not_in_remote"
	FallbackRemoteUnavailable  FallbackReason = "remote_unavailable"
	FallbackRemoteNotAttempted FallbackReason = "remote_not_attempted"
)

// Stable reason values for blocked / not-found responses. Existing
// values do not change shape; adding values is a non-breaking
// change.
const (
	ReasonRedactionFailed  = "redaction_failed"
	ReasonRefNotResolvable = "ref_not_resolvable"
	ReasonRefUnsafe        = "ref_unsafe"
)

// Output is the JSON contract the SKILL.md body parses. Field tags
// use omitempty so each mode produces a clean, mode-shaped object
// without empty-string noise the agent might accidentally render.
type Output struct {
	Mode           Mode            `json:"mode"`
	HumanText      string          `json:"human_text,omitempty"`
	CommitMetadata *CommitMetadata `json:"commit_metadata,omitempty"`
	DiffExcerpt    string          `json:"diff_excerpt,omitempty"`
	FallbackReason FallbackReason  `json:"fallback_reason,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Message        string          `json:"message,omitempty"`
}

// CommitMetadata is the shape returned in git-only mode so the
// agent can render the commit header without re-running git.
type CommitMetadata struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// Service runs the layered fallback. Construct via NewService.
type Service struct{}

// NewService returns a stateless service.
func NewService() *Service { return &Service{} }

// Input narrows the surface callers must provide.
type Input struct {
	RepoPath string
	Ref      string
}

// safeCommitSHA matches a 7..40 character lowercase-hex string.
var safeCommitSHA = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// safeBranchOrTag covers ASCII branch and tag names without shell
// metacharacters. The 1..128 length cap matches the SKILL.md side
// check; defense in depth keeps the CLI from depending on the
// agent doing its job.
var safeBranchOrTag = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,128}$`)

// IsSafeRef reports whether ref matches one of the three accepted
// shapes: HEAD, a commit SHA, or a simple branch / tag name. The
// CLI also runs `git rev-parse --verify` on safe refs; the static
// check up front avoids spawning git for input we already know to
// reject.
//
// Path-shaped inputs (leading `/`, `..` traversal) and shell-meta
// characters are rejected here, before anything reaches git.
func IsSafeRef(ref string) bool {
	if ref == "HEAD" {
		return true
	}
	if safeCommitSHA.MatchString(ref) {
		return true
	}
	if !safeBranchOrTag.MatchString(ref) {
		return false
	}
	if strings.HasPrefix(ref, "-") {
		return false
	}
	if strings.HasPrefix(ref, "/") {
		return false
	}
	if strings.Contains(ref, "..") {
		return false
	}
	return true
}

// Explain resolves the requested ref and returns the best available
// explanation shape. The current implementation returns git-only
// output after ref resolution; local and remote provenance can be
// added without changing the JSON shape.
func (s *Service) Explain(ctx context.Context, in Input) (*Output, error) {
	if !IsSafeRef(in.Ref) {
		return &Output{
			Mode:    ModeNotFound,
			Reason:  ReasonRefUnsafe,
			Message: fmt.Sprintf("ref %q is not a supported shape (use HEAD, a commit SHA, or a simple branch / tag name)", in.Ref),
		}, nil
	}

	repo, err := git.OpenRepo(in.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}

	hash, err := repo.ResolveRef(ctx, in.Ref)
	if err != nil {
		return &Output{
			Mode:    ModeNotFound,
			Reason:  ReasonRefNotResolvable,
			Message: fmt.Sprintf("ref %q does not resolve in this repo", in.Ref),
		}, nil
	}

	return gitOnly(ctx, repo, hash)
}

// gitOnly populates commit_metadata and the redacted diff_excerpt
// for layer 3. Redaction is fail-closed: a redactor error returns
// mode: blocked rather than leaking unredacted content or
// masquerading as not-found. Order is redact-then-truncate so a
// secret straddling the byte cap cannot leak its prefix.
func gitOnly(ctx context.Context, repo *git.Repo, hash string) (*Output, error) {
	meta, err := readCommitMetadata(ctx, repo, hash)
	if err != nil {
		return nil, fmt.Errorf("read commit metadata: %w", err)
	}

	diff, err := repo.DiffForCommit(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("read diff: %w", err)
	}

	redacted, err := redact.Bytes(diff)
	if err != nil {
		return &Output{
			Mode:    ModeBlocked,
			Reason:  ReasonRedactionFailed,
			Message: "redactor failed to process the diff for this commit; refusing to render unredacted content",
		}, nil
	}

	bounded, truncated := boundDiff(redacted, maxDiffBytes)
	excerpt := string(bounded)
	if truncated {
		excerpt += truncatedMarker
	}

	return &Output{
		Mode:           ModeGitOnly,
		CommitMetadata: meta,
		DiffExcerpt:    excerpt,
		FallbackReason: FallbackRemoteNotAttempted,
	}, nil
}

// boundDiff truncates b to at most max bytes. Returns the
// (possibly shorter) slice and whether truncation occurred so the
// caller can append a stable marker.
func boundDiff(b []byte, max int) ([]byte, bool) {
	if len(b) <= max {
		return b, false
	}
	return b[:max], true
}

// readCommitMetadata fetches commit hash, author, date, subject in
// a single git invocation. The format string uses %n separators so
// the four fields parse cleanly even when any of them contain
// spaces. Returns a non-nil pointer because every git-only output
// carries metadata.
func readCommitMetadata(ctx context.Context, repo *git.Repo, hash string) (*CommitMetadata, error) {
	const format = "%H%n%an%n%ai%n%s"
	out, err := repo.CommitFormat(ctx, hash, format)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(out, "\n", 4)
	if len(parts) < 4 {
		return nil, errors.New("unexpected git show -s output shape")
	}
	return &CommitMetadata{
		Hash:    parts[0],
		Author:  parts[1],
		Date:    parts[2],
		Subject: parts[3],
	}, nil
}
