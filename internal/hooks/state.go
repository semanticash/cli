package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/broker"
)

// ErrNoCaptureState is returned when no capture state file exists for a session.
var ErrNoCaptureState = errors.New("no capture state")

// CaptureState tracks the read position for an active transcript.
// Created when a turn starts, advanced on each capture, and removed after the
// turn finishes. Stored globally at ~/.semantica/capture/.
//
// State file naming uses Key(): parent states are keyed by SessionID,
// subagent states by a derived identifier (e.g., the subagent transcript
// basename), so each transcript gets its own independent offset.
type CaptureState struct {
	SessionID        string `json:"session_id"`
	StateKey         string `json:"state_key,omitempty"` // Override key; defaults to SessionID.
	Provider         string `json:"provider"`
	TranscriptRef    string `json:"transcript_ref"`
	TranscriptOffset int    `json:"transcript_offset"`
	Timestamp        int64  `json:"timestamp"`

	TurnID            string `json:"turn_id,omitempty"`
	PromptSubmittedAt int64  `json:"prompt_submitted_at,omitempty"`
	CWD               string `json:"cwd,omitempty"` // working directory from hook payload
}

// Key returns the identifier used for the state file name.
// Subagent states set StateKey explicitly; parent states fall back to SessionID.
func (s *CaptureState) Key() string {
	if s.StateKey != "" {
		return s.StateKey
	}
	return s.SessionID
}

// captureDir returns the global capture state directory.
func captureDir() (string, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "capture"), nil
}

// stateFilePath returns the path for a session's capture state file.
func stateFilePath(sessionID string) (string, error) {
	dir, err := captureDir()
	if err != nil {
		return "", err
	}
	// Sanitize session ID to prevent path traversal.
	safe := filepath.Base(sessionID)
	return filepath.Join(dir, fmt.Sprintf("capture-%s.json", safe)), nil
}

// SaveCaptureState writes a capture state file atomically.
func SaveCaptureState(state *CaptureState) error {
	if state.Key() == "" {
		return fmt.Errorf("empty capture state key")
	}

	path, err := stateFilePath(state.Key())
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir capture dir: %w", err)
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal capture state: %w", err)
	}

	// Atomic write: write to temp file, then rename.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write capture state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename capture state: %w", err)
	}
	return nil
}

// LoadCaptureState reads a capture state file for the given session.
// Returns ErrNoCaptureState if the file does not exist.
func LoadCaptureState(sessionID string) (*CaptureState, error) {
	path, err := stateFilePath(sessionID)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCaptureState
		}
		return nil, fmt.Errorf("read capture state: %w", err)
	}

	var state CaptureState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal capture state: %w", err)
	}
	return &state, nil
}

// LoadActiveCaptureStates scans all capture state files. Used by commit-time
// catch-up to flush all active sessions across all repos.
func LoadActiveCaptureStates() ([]*CaptureState, error) {
	dir, err := captureDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read capture dir: %w", err)
	}

	var states []*CaptureState
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "capture-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var state CaptureState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		states = append(states, &state)
	}
	return states, nil
}

// DeleteCaptureState removes a capture state file by session ID.
func DeleteCaptureState(sessionID string) error {
	return DeleteCaptureStateByKey(sessionID)
}

// DeleteCaptureStateByKey removes a capture state file by its key.
func DeleteCaptureStateByKey(key string) error {
	path, err := stateFilePath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete capture state: %w", err)
	}
	return nil
}

// LoadCaptureStateByKey reads a capture state file by its key.
// Returns ErrNoCaptureState if the file does not exist.
func LoadCaptureStateByKey(key string) (*CaptureState, error) {
	path, err := stateFilePath(key)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCaptureState
		}
		return nil, fmt.Errorf("read capture state: %w", err)
	}

	var state CaptureState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal capture state: %w", err)
	}
	return &state, nil
}
