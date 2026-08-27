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
	// Freeze identifies an optional workspace observation paired with the commit.
	Freeze *freezeHandoff
}

// freezeHandoff identifies a frozen workspace observation.
type freezeHandoff struct {
	ObservationID string `json:"observation_id"`
	Tree          string `json:"tree"`
	Ref           string `json:"ref"`
	EventCursor   int64  `json:"event_cursor"`
}

// commitReceipt records a commit whose checkpoint link is pending.
type commitReceipt struct {
	CheckpointID string         `json:"checkpoint_id"`
	CreatedAt    int64          `json:"created_at"`
	CommitSHA    string         `json:"commit_sha"`
	Freeze       *freezeHandoff `json:"freeze,omitempty"`
}

func commitReceiptsDir(semDir string) string {
	return filepath.Join(semDir, "commit-receipts")
}

func readCommitReceiptEntries(semDir string) (string, []os.DirEntry, error) {
	dir := commitReceiptsDir(semDir)
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return dir, nil, nil
		}
		return dir, nil, err
	}
	if !info.IsDir() {
		return dir, nil, fmt.Errorf("not a directory")
	}
	entries, err := os.ReadDir(dir)
	return dir, entries, err
}

// writeCommitHandoff stores commit checkpoint state for post-commit processing.
// The checkpoint ID remains first for commit-msg compatibility.
func writeCommitHandoff(semDir string, h commitHandoff) error {
	line := fmt.Sprintf("%s|%d|%s|%s", h.CheckpointID, h.CreatedAt, h.Tree, h.Head)
	if h.Freeze != nil {
		line += fmt.Sprintf("|%s|%s|%s|%d",
			h.Freeze.ObservationID, h.Freeze.Tree, h.Freeze.Ref, h.Freeze.EventCursor)
	}
	line += "\n"
	return platform.WriteFileAtomic(util.PreCommitCheckpointPath(semDir), []byte(line), 0o644)
}

// readCommitHandoff returns ok=false when no handoff exists. Unreadable or
// malformed handoffs return an error and remain available for repair.
func readCommitHandoff(semDir string) (commitHandoff, bool, error) {
	raw, err := os.ReadFile(util.PreCommitCheckpointPath(semDir))
	if err != nil {
		if os.IsNotExist(err) {
			return commitHandoff{}, false, nil
		}
		return commitHandoff{}, false, fmt.Errorf("read handoff: %w", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return commitHandoff{}, false, fmt.Errorf("handoff is empty")
	}
	fields := strings.Split(trimmed, "|")
	if len(fields) != 4 && len(fields) != 8 {
		return commitHandoff{}, false, fmt.Errorf("handoff has %d fields, want 4 or 8", len(fields))
	}
	checkpointID := strings.TrimSpace(fields[0])
	if checkpointID == "" {
		return commitHandoff{}, false, fmt.Errorf("handoff missing checkpoint id")
	}
	createdAt, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
	if err != nil || createdAt <= 0 {
		return commitHandoff{}, false, fmt.Errorf("handoff created_at invalid: %q", fields[1])
	}
	tree := strings.TrimSpace(fields[2])
	if tree == "" {
		return commitHandoff{}, false, fmt.Errorf("handoff missing staged tree")
	}
	// fields[3] (parent HEAD) may be empty for an unborn branch.
	h := commitHandoff{CheckpointID: checkpointID, CreatedAt: createdAt, Tree: tree, Head: strings.TrimSpace(fields[3])}
	if len(fields) == 8 {
		cursor, cerr := strconv.ParseInt(strings.TrimSpace(fields[7]), 10, 64)
		if cerr != nil {
			return commitHandoff{}, false, fmt.Errorf("handoff freeze cursor invalid: %q", fields[7])
		}
		freeze := freezeHandoff{
			ObservationID: strings.TrimSpace(fields[4]),
			Tree:          strings.TrimSpace(fields[5]),
			Ref:           strings.TrimSpace(fields[6]),
			EventCursor:   cursor,
		}
		if verr := validateFreezePairing(freeze, createdAt); verr != nil {
			return commitHandoff{}, false, fmt.Errorf("handoff freeze pairing invalid: %w", verr)
		}
		h.Freeze = &freeze
	}
	return h, true, nil
}

func removeCommitHandoff(semDir string) {
	_ = os.Remove(util.PreCommitCheckpointPath(semDir))
}

// promoteToReceipt persists a committed receipt before removing its handoff.
func promoteToReceipt(semDir, sha string, h commitHandoff) (commitReceipt, error) {
	r := commitReceipt{CheckpointID: h.CheckpointID, CreatedAt: h.CreatedAt, CommitSHA: sha, Freeze: h.Freeze}
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
	if r.Freeze != nil {
		if err := validateFreezePairing(*r.Freeze, r.CreatedAt); err != nil {
			return commitReceipt{}, fmt.Errorf("invalid freeze pairing: %w", err)
		}
	}
	return r, nil
}

// listCommitReceipts returns receipts in commit order. Invalid receipts block
// processing and remain on disk for repair.
func listCommitReceipts(semDir string) ([]commitReceipt, error) {
	dir, entries, err := readCommitReceiptEntries(semDir)
	if err != nil {
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
	dir, entries, err := readCommitReceiptEntries(semDir)
	if err != nil {
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

// linkReceipt idempotently creates the checkpoint pair and commit link.
func linkReceipt(ctx context.Context, h *sqlstore.Handle, repoID string, r commitReceipt) error {
	switch commit, err := h.Queries.GetCheckpointByID(ctx, r.CheckpointID); {
	case errors.Is(err, sql.ErrNoRows):
		if r.Freeze != nil {
			if err := insertPairedCheckpoints(ctx, h, repoID, r.CheckpointID, r.CreatedAt, *r.Freeze); err != nil {
				return err
			}
		} else if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
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
	case err != nil:
		return err
	default:
		// Existing paired checkpoints must agree with the receipt.
		if r.Freeze != nil {
			if err := verifyObservationPairing(ctx, h, commit, *r.Freeze); err != nil {
				return fmt.Errorf("freeze pairing disagreement for %s: %w", r.CommitSHA, err)
			}
		}
	}
	return h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   r.CommitSHA,
		RepositoryID: repoID,
		CheckpointID: r.CheckpointID,
		LinkedAt:     time.Now().UnixMilli(),
	})
}

// verifyObservationPairing validates an existing workspace observation pair.
func verifyObservationPairing(ctx context.Context, h *sqlstore.Handle, commit sqldb.Checkpoint, freeze freezeHandoff) error {
	obs, err := h.Queries.GetCheckpointByID(ctx, freeze.ObservationID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("observation %s missing", freeze.ObservationID)
	}
	if err != nil {
		return err
	}
	if obs.Trigger.String != "workspace_freeze" {
		return fmt.Errorf("observation %s has trigger %q", freeze.ObservationID, obs.Trigger.String)
	}
	if obs.RepositorySequence >= commit.RepositorySequence {
		return fmt.Errorf("observation seq %d not below commit seq %d", obs.RepositorySequence, commit.RepositorySequence)
	}
	if !obs.EventCursor.Valid || obs.EventCursor.Int64 != freeze.EventCursor {
		return fmt.Errorf("observation cursor %v != pinned %d", obs.EventCursor, freeze.EventCursor)
	}
	return nil
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
