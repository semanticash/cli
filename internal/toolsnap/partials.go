package toolsnap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PendingPartialRecord preserves canonical inputs for a groupless partial.
// Retries rebuild evidence from this record instead of a reparsed hook.
type PendingPartialRecord struct {
	Key            ToolKey `json:"key"`
	EventID        string  `json:"event_id"`
	Reason         string  `json:"reason"`
	ToolName       string  `json:"tool_name"`
	CommandSummary string  `json:"command_summary,omitempty"`
	Timestamp      int64   `json:"timestamp"`
}

func (p PendingPartialRecord) validate() error {
	if err := p.Key.validate(); err != nil {
		return err
	}
	// Production event IDs are SHA-256 digests and safe filenames.
	if !isHexDigest(p.EventID) {
		return fmt.Errorf("toolsnap: pending partial event id must be a 64-char lowercase hex digest")
	}
	if p.Reason == "" || p.ToolName == "" || p.Timestamp <= 0 {
		return fmt.Errorf("toolsnap: pending partial record incomplete")
	}
	return nil
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (r *Registry) partialsDir() string { return filepath.Join(r.dir, "partials") }

// LoadOrRecordPendingPartial creates or returns an event's first record.
// Existing records are validated before use.
func (r *Registry) LoadOrRecordPendingPartial(rec PendingPartialRecord) (PendingPartialRecord, error) {
	if err := rec.validate(); err != nil {
		return PendingPartialRecord{}, err
	}
	dir := r.partialsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return PendingPartialRecord{}, fmt.Errorf("toolsnap: partials dir: %w", err)
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return PendingPartialRecord{}, fmt.Errorf("toolsnap: encode pending partial: %w", err)
	}
	path := filepath.Join(dir, rec.EventID)
	f, err := os.CreateTemp(dir, rec.EventID+".tmp-*")
	if err != nil {
		return PendingPartialRecord{}, fmt.Errorf("toolsnap: pending partial temp: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return PendingPartialRecord{}, fmt.Errorf("toolsnap: write pending partial: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return PendingPartialRecord{}, fmt.Errorf("toolsnap: sync pending partial: %w", err)
	}
	if err := f.Close(); err != nil {
		return PendingPartialRecord{}, fmt.Errorf("toolsnap: close pending partial: %w", err)
	}
	if err := os.Link(tmp, path); err != nil {
		if !os.IsExist(err) {
			return PendingPartialRecord{}, fmt.Errorf("toolsnap: publish pending partial: %w", err)
		}
		return r.readPendingPartial(path, rec.EventID)
	}
	return rec, nil
}

// PendingPartialRecords lists the records for recovery sweeps.
func (r *Registry) PendingPartialRecords() ([]PendingPartialRecord, error) {
	entries, err := os.ReadDir(r.partialsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("toolsnap: list pending partials: %w", err)
	}
	var out []PendingPartialRecord
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		rec, err := r.readPendingPartial(filepath.Join(r.partialsDir(), e.Name()), e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// RemovePendingPartial deletes a recovery record after its link is durable.
func (r *Registry) RemovePendingPartial(eventID string) error {
	if !isHexDigest(eventID) {
		return fmt.Errorf("toolsnap: pending partial event id must be a 64-char lowercase hex digest")
	}
	err := os.Remove(filepath.Join(r.partialsDir(), eventID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("toolsnap: remove pending partial: %w", err)
	}
	return nil
}

func (r *Registry) readPendingPartial(path, eventID string) (PendingPartialRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PendingPartialRecord{}, fmt.Errorf("toolsnap: read pending partial: %w", err)
	}
	var rec PendingPartialRecord
	if json.Unmarshal(raw, &rec) != nil || rec.validate() != nil || rec.EventID != eventID {
		return PendingPartialRecord{}, fmt.Errorf("%w: malformed pending partial %s", ErrRegistryCorrupt, eventID)
	}
	return rec, nil
}
