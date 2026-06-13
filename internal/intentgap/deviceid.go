package intentgap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/semanticash/cli/internal/util"
)

// DeviceIDFileName is the basename of the file under AppConfigDir that
// holds the stable per-machine random device identifier.
const DeviceIDFileName = "device_id"

// LoadOrCreateDeviceID returns a stable random identifier for this
// machine. The identifier is generated once on first call, persisted
// under the user-global Semantica config directory, and reused on
// every subsequent call. It is audit/context metadata only - the
// server intentionally excludes it from upload deduplication and from
// the canonical payload hash.
//
// If the existing file is unreadable or holds an invalid UUID, a
// fresh one is generated and persisted in its place.
func LoadOrCreateDeviceID() (string, error) {
	dir, err := util.AppConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return loadOrCreateDeviceIDIn(dir)
}

// loadOrCreateDeviceIDIn isolates filesystem operations so tests can
// hand in a temp dir without touching the real user config path.
func loadOrCreateDeviceIDIn(dir string) (string, error) {
	path := filepath.Join(dir, DeviceIDFileName)

	if existing, ok := readValidDeviceID(path); ok {
		return existing, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}
	id := uuid.NewString()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write device id: %w", err)
	}
	return id, nil
}

// readValidDeviceID returns the persisted UUID when the file exists
// and parses cleanly. Any I/O or parse failure is treated as "needs
// regeneration" rather than fatal: the device id is audit metadata,
// not a security boundary, and regenerating on corruption is safer
// than refusing uploads.
func readValidDeviceID(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "", false
	}
	if _, err := uuid.Parse(trimmed); err != nil {
		return "", false
	}
	return trimmed, true
}
