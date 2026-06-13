package intentgap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// First call generates a new UUID and persists it.
func TestLoadOrCreateDeviceIDIn_GeneratesOnEmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := loadOrCreateDeviceIDIn(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("returned id is not a UUID: %q (%v)", got, err)
	}

	stored, err := os.ReadFile(filepath.Join(dir, DeviceIDFileName))
	if err != nil {
		t.Fatalf("read persisted: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("device_id file should not be empty")
	}
}

// Second call returns the same UUID instead of regenerating.
func TestLoadOrCreateDeviceIDIn_StableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateDeviceIDIn(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := loadOrCreateDeviceIDIn(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("device id changed across calls: %s vs %s", first, second)
	}
}

// A corrupted on-disk file (not a UUID) triggers regeneration rather
// than failing the upload path. Device id is audit metadata, not a
// security boundary.
func TestLoadOrCreateDeviceIDIn_RegeneratesOnCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DeviceIDFileName)
	if err := os.WriteFile(path, []byte("not-a-uuid"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadOrCreateDeviceIDIn(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("regenerated id is not a UUID: %q", got)
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted: %v", err)
	}
	if string(stored) == "not-a-uuid" {
		t.Fatal("corrupted file should have been overwritten")
	}
}

// An empty file is treated the same as a missing one: regenerate.
func TestLoadOrCreateDeviceIDIn_RegeneratesOnEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DeviceIDFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadOrCreateDeviceIDIn(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("returned id is not a UUID: %q", got)
	}
}
