package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/platform"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// commitHandoff binds a checkpoint to the commit being created.
type commitHandoff struct {
	CheckpointID string
	CreatedAt    int64  // Preserved as the checkpoint creation time.
	Tree         string // Staged tree recorded by pre-commit.
	Head         string // HEAD recorded by pre-commit.
}

// commitReceipt records a commit whose checkpoint link is still pending.
type commitReceipt struct {
	CheckpointID string `json:"checkpoint_id"`
	CreatedAt    int64  `json:"created_at"`
	CommitSHA    string `json:"commit_sha"`
}

func commitReceiptsDir(semDir string) string {
	return filepath.Join(semDir, "commit-receipts")
}

// writeCommitHandoff stores checkpoint_id|created_at|tree|head. The checkpoint
// ID remains first for compatibility with the commit-msg hook.
func writeCommitHandoff(semDir string, h commitHandoff) error {
	line := fmt.Sprintf("%s|%d|%s|%s\n", h.CheckpointID, h.CreatedAt, h.Tree, h.Head)
	return platform.WriteFileAtomic(util.PreCommitCheckpointPath(semDir), []byte(line), 0o644)
}

// readCommitHandoff reads the current pre-commit handoff.
func readCommitHandoff(semDir string) (commitHandoff, bool) {
	raw, err := os.ReadFile(util.PreCommitCheckpointPath(semDir))
	if err != nil {
		return commitHandoff{}, false
	}
	fields := strings.Split(strings.TrimSpace(string(raw)), "|")
	if len(fields) == 0 || strings.TrimSpace(fields[0]) == "" {
		return commitHandoff{}, false
	}
	h := commitHandoff{CheckpointID: strings.TrimSpace(fields[0])}
	if len(fields) > 1 {
		h.CreatedAt, _ = strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
	}
	if len(fields) > 2 {
		h.Tree = strings.TrimSpace(fields[2])
	}
	if len(fields) > 3 {
		h.Head = strings.TrimSpace(fields[3])
	}
	return h, true
}

func removeCommitHandoff(semDir string) {
	_ = os.Remove(util.PreCommitCheckpointPath(semDir))
}

// promoteToReceipt persists a committed receipt before removing its handoff.
func promoteToReceipt(semDir, sha string, h commitHandoff) (commitReceipt, error) {
	r := commitReceipt{CheckpointID: h.CheckpointID, CreatedAt: h.CreatedAt, CommitSHA: sha}
	if err := os.MkdirAll(commitReceiptsDir(semDir), 0o755); err != nil {
		return r, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	if err := platform.WriteFileAtomic(filepath.Join(commitReceiptsDir(semDir), sha+".json"), data, 0o644); err != nil {
		return r, err
	}
	removeCommitHandoff(semDir)
	return r, nil
}

// validateReceiptFile reads and validates a <commit_sha>.json receipt.
func validateReceiptFile(dir, name string) (commitReceipt, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return commitReceipt{}, fmt.Errorf("unreadable: %w", err)
	}
	var r commitReceipt
	if err := json.Unmarshal(data, &r); err != nil {
		return commitReceipt{}, fmt.Errorf("malformed JSON: %w", err)
	}
	if r.CheckpointID == "" {
		return commitReceipt{}, errors.New("missing checkpoint_id")
	}
	if r.CreatedAt <= 0 {
		return commitReceipt{}, fmt.Errorf("non-positive created_at (%d)", r.CreatedAt)
	}
	if !git.IsFullCommitHash(r.CommitSHA) {
		return commitReceipt{}, fmt.Errorf("commit_sha %q is not a full commit hash", r.CommitSHA)
	}
	if name != r.CommitSHA+".json" {
		return commitReceipt{}, fmt.Errorf("filename does not match commit_sha (expected %s.json)", r.CommitSHA)
	}
	return r, nil
}

// listCommitReceipts returns receipts in commit order. Invalid receipts block
// processing and remain on disk for repair.
func listCommitReceipts(semDir string) ([]commitReceipt, error) {
	dir := commitReceiptsDir(semDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []commitReceipt
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// Receipt files must be regular files; do not follow symlinks.
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("commit receipt %s: not a regular file", e.Name())
		}
		r, verr := validateReceiptFile(dir, e.Name())
		if verr != nil {
			return nil, fmt.Errorf("commit receipt %s: %w", e.Name(), verr)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].CommitSHA < out[j].CommitSHA
	})
	return out, nil
}

// ReceiptProblem is one invalid commit receipt, reported by doctor.
type ReceiptProblem struct {
	File   string
	Reason string
}

// InspectCommitReceipts returns invalid receipts and directory read failures.
func InspectCommitReceipts(semDir string) []ReceiptProblem {
	dir := commitReceiptsDir(semDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []ReceiptProblem{{File: "commit-receipts/", Reason: fmt.Sprintf("directory unreadable: %v", err)}}
	}
	var problems []ReceiptProblem
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if !e.Type().IsRegular() {
			problems = append(problems, ReceiptProblem{File: e.Name(), Reason: "not a regular file"})
			continue
		}
		if _, verr := validateReceiptFile(dir, e.Name()); verr != nil {
			problems = append(problems, ReceiptProblem{File: e.Name(), Reason: verr.Error()})
		}
	}
	sort.Slice(problems, func(i, j int) bool { return problems[i].File < problems[j].File })
	return problems
}

// commitReceiptsPending reports whether a receipt awaits processing.
func commitReceiptsPending(semDir string) (bool, error) {
	receipts, err := listCommitReceipts(semDir)
	if err != nil {
		return false, err
	}
	return len(receipts) > 0, nil
}

// removeCommitReceipt deletes a receipt without trusting path components in
// the supplied SHA.
func removeCommitReceipt(semDir, sha string) error {
	path := filepath.Join(commitReceiptsDir(semDir), filepath.Base(sha)+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// commitMatchesHandoffParent accepts normal commits and amendments.
func commitMatchesHandoffParent(ctx context.Context, repo *git.Repo, sha, recordedHead string) (bool, error) {
	commitParent, _, err := repo.CommitParent(ctx, sha)
	if err != nil {
		return false, err
	}
	if commitParent == recordedHead {
		return true, nil
	}
	if recordedHead == "" {
		return false, nil // nothing to amend from an unborn branch
	}
	amendParent, _, err := repo.CommitParent(ctx, recordedHead)
	if err != nil {
		return false, err
	}
	return commitParent == amendParent, nil
}

// linkReceiptFn allows tests to inject a receipt-link failure.
var linkReceiptFn = linkReceipt

// linkReceipt idempotently creates the checkpoint and commit link.
func linkReceipt(ctx context.Context, h *sqlstore.Handle, repoID string, r commitReceipt) error {
	if _, err := h.Queries.GetCheckpointByID(ctx, r.CheckpointID); errors.Is(err, sql.ErrNoRows) {
		if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
			CheckpointID: r.CheckpointID,
			RepositoryID: repoID,
			CreatedAt:    r.CreatedAt,
			Kind:         string(CheckpointAuto),
			Trigger:      sql.NullString{String: "commit", Valid: true},
			Message:      sql.NullString{String: "Auto checkpoint", Valid: true},
			ManifestHash: sql.NullString{},
			SizeBytes:    sql.NullInt64{},
			Status:       "pending",
			CompletedAt:  sql.NullInt64{},
		}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   r.CommitSHA,
		RepositoryID: repoID,
		CheckpointID: r.CheckpointID,
		LinkedAt:     time.Now().UnixMilli(),
	})
}

// drainCommitReceipts links receipts in order under the repository lock. It
// stops on the first failure so newer checkpoints cannot overtake older ones.
func drainCommitReceipts(ctx context.Context, repoRoot string) error {
	semDir := filepath.Join(repoRoot, ".semantica")
	receipts, err := listCommitReceipts(semDir)
	if err != nil {
		util.AppendActivityLog(semDir, "worker warning: list commit receipts failed: %v", err)
		return fmt.Errorf("list commit receipts: %w", err)
	}
	if len(receipts) == 0 {
		return nil
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		util.AppendActivityLog(semDir, "worker warning: open db for receipt drain failed (kept): %v", err)
		return fmt.Errorf("open db for receipt drain: %w", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoID, err := sqlstore.EnsureRepository(ctx, h.Queries, repoRoot)
	if err != nil {
		util.AppendActivityLog(semDir, "worker warning: ensure repo for receipt drain failed: %v", err)
		return fmt.Errorf("ensure repo for receipt drain: %w", err)
	}

	// Stop at the first failure to preserve receipt order.
	for _, r := range receipts {
		if err := linkReceiptFn(ctx, h, repoID, r); err != nil {
			util.AppendActivityLog(semDir, "worker warning: link receipt %s failed (kept): %v", r.CommitSHA, err)
			return fmt.Errorf("link receipt %s: %w", r.CommitSHA, err)
		}
		if err := removeCommitReceipt(semDir, r.CommitSHA); err != nil {
			util.AppendActivityLog(semDir, "worker warning: remove receipt %s failed: %v", r.CommitSHA, err)
		}
	}
	return nil
}
