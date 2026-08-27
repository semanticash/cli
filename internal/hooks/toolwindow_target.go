package hooks

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/toolsnap"
)

// toolWindowTargetVersion is the persisted target schema version.
const toolWindowTargetVersion = 1

// toolWindowTargetTTL matches the maximum tool-window lifetime.
var toolWindowTargetTTL = toolsnap.DefaultStaleWindowAge

// toolWindowNow returns Unix milliseconds and is replaceable in tests.
var toolWindowNow = func() int64 { return time.Now().UnixMilli() }

// toolWindowReceiptKey identifies one provider tool window.
type toolWindowReceiptKey struct {
	Provider  string
	SessionID string
	TurnID    string
	ToolUseID string
}

func (k toolWindowReceiptKey) valid() bool {
	return k.Provider != "" && k.SessionID != "" && k.ToolUseID != ""
}

// fileName hashes length-prefixed fields into a traversal-safe filename.
func (k toolWindowReceiptKey) fileName() string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, s := range []string{k.Provider, k.SessionID, k.TurnID, k.ToolUseID} {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(s))
	}
	return "toolwindow-" + hex.EncodeToString(h.Sum(nil)) + ".json"
}

// ToolWindowTargetRecord stores the repository selected by a pre-tool hook.
type ToolWindowTargetRecord struct {
	Version      int    `json:"version"`
	CreatedAt    int64  `json:"created_at"` // unix ms
	Provider     string `json:"provider"`
	SessionID    string `json:"session_id"`
	TurnID       string `json:"turn_id,omitempty"`
	ToolUseID    string `json:"tool_use_id"`
	RepoPath     string `json:"repo_path"`
	RepositoryID string `json:"repository_id"`
}

func toolWindowTargetPath(key toolWindowReceiptKey) (string, error) {
	dir, err := captureDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key.fileName()), nil
}

// SaveToolWindowTarget persists the pre-hook-selected target for a tool window.
// It requires a complete window identity and a resolved repository.
func SaveToolWindowTarget(key toolWindowReceiptKey, repoPath, repositoryID string) error {
	if !key.valid() {
		return fmt.Errorf("tool window target: incomplete window identity")
	}
	if repoPath == "" || repositoryID == "" {
		return fmt.Errorf("tool window target: empty repository")
	}
	path, err := toolWindowTargetPath(key)
	if err != nil {
		return err
	}
	rec := ToolWindowTargetRecord{
		Version: toolWindowTargetVersion, CreatedAt: toolWindowNow(),
		Provider: key.Provider, SessionID: key.SessionID, TurnID: key.TurnID, ToolUseID: key.ToolUseID,
		RepoPath: repoPath, RepositoryID: repositoryID,
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir capture dir: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal tool window target: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "toolwindow-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp tool window target: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write tool window target: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close tool window target: %w", err)
	}
	if err := platform.SafeRename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename tool window target: %w", err)
	}
	return nil
}

// LoadToolWindowTarget returns a validated target. A missing target returns
// (nil, nil); an invalid or expired target returns an error.
func LoadToolWindowTarget(key toolWindowReceiptKey) (*ToolWindowTargetRecord, error) {
	path, err := toolWindowTargetPath(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tool window target: %w", err)
	}
	var rec ToolWindowTargetRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal tool window target: %w", err)
	}
	if rec.Version != toolWindowTargetVersion {
		return nil, fmt.Errorf("tool window target: unexpected version %d", rec.Version)
	}
	if rec.Provider != key.Provider || rec.SessionID != key.SessionID ||
		rec.TurnID != key.TurnID || rec.ToolUseID != key.ToolUseID {
		return nil, fmt.Errorf("tool window target: identity mismatch")
	}
	if rec.RepoPath == "" || rec.RepositoryID == "" {
		return nil, fmt.Errorf("tool window target: empty repository")
	}
	if age := toolWindowNow() - rec.CreatedAt; age < 0 || age > toolWindowTargetTTL.Milliseconds() {
		return nil, fmt.Errorf("tool window target: expired (age %dms)", age)
	}
	return &rec, nil
}

// DeleteToolWindowTarget removes a target. A missing file is not an error.
func DeleteToolWindowTarget(key toolWindowReceiptKey) error {
	path, err := toolWindowTargetPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove tool window target: %w", err)
	}
	return nil
}

// SweepToolWindowTargets removes expired or malformed target files. It skips
// non-regular entries and returns any read or removal errors.
func SweepToolWindowTargets() (int, error) {
	dir, err := captureDir()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read capture dir: %w", err)
	}
	removed := 0
	var errs []error
	cutoff := toolWindowNow() - toolWindowTargetTTL.Milliseconds()
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, "toolwindow-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", name, rerr))
			continue
		}
		var rec ToolWindowTargetRecord
		// Records without a valid creation time cannot be used.
		stale := json.Unmarshal(data, &rec) != nil || rec.CreatedAt == 0 || rec.CreatedAt < cutoff
		if !stale {
			continue
		}
		if derr := os.Remove(path); derr != nil && !os.IsNotExist(derr) {
			errs = append(errs, fmt.Errorf("remove %s: %w", name, derr))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
