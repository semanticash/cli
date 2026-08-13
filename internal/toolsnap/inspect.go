package toolsnap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RegistrySnapshot is a read-only view used by diagnostics and dry runs.
type RegistrySnapshot struct {
	// Exists reports whether the registry directory is present.
	Exists              bool
	Windows             []PendingToolSnapshot
	Groups              map[string]GroupMeta
	Finals              map[string]GroupFinal
	Partials            []PendingPartialRecord
	Tombstones          []Tombstone
	MalformedTombstones []string
}

// InspectRegistry validates registry state and overlays receipts in memory.
// It does not create, consume, or publish files.
func InspectRegistry(semDir string) (RegistrySnapshot, error) {
	snap := RegistrySnapshot{}
	dir := filepath.Join(semDir, "tool-windows")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return snap, nil
		}
		return snap, fmt.Errorf("toolsnap: inspect registry: %w", err)
	}
	snap.Exists = true

	r := &Registry{dir: dir}
	state, _, _, err := r.loadState(nil)
	if err != nil {
		return snap, err
	}
	snap.Windows = state.Windows
	snap.Groups = state.Groups
	snap.Finals = state.Finals

	entries, err := os.ReadDir(filepath.Join(dir, "partials"))
	if err != nil && !os.IsNotExist(err) {
		return snap, fmt.Errorf("toolsnap: inspect pending partials: %w", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "partials", e.Name()))
		if err != nil {
			return snap, fmt.Errorf("toolsnap: inspect pending partial: %w", err)
		}
		var rec PendingPartialRecord
		if json.Unmarshal(raw, &rec) != nil || rec.validate() != nil || rec.EventID != e.Name() {
			return snap, fmt.Errorf("%w: malformed pending partial %s", ErrRegistryCorrupt, e.Name())
		}
		snap.Partials = append(snap.Partials, rec)
	}

	entries, err = os.ReadDir(filepath.Join(dir, "tombstones"))
	if err != nil && !os.IsNotExist(err) {
		return snap, fmt.Errorf("toolsnap: inspect tombstones: %w", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "tombstones", e.Name()))
		if err != nil {
			snap.MalformedTombstones = append(snap.MalformedTombstones, e.Name())
			continue
		}
		var t Tombstone
		if json.Unmarshal(raw, &t) != nil || t.validate() != nil || t.Key.hash() != e.Name() {
			snap.MalformedTombstones = append(snap.MalformedTombstones, e.Name())
			continue
		}
		snap.Tombstones = append(snap.Tombstones, t)
	}
	return snap, nil
}

// CompleteGroups returns groups whose members are all complete.
func (s RegistrySnapshot) CompleteGroups() []PendingFinalization {
	byGroup := map[string][]PendingToolSnapshot{}
	active := map[string]bool{}
	for _, w := range s.Windows {
		byGroup[w.GroupID] = append(byGroup[w.GroupID], w)
		if w.Status == "active" {
			active[w.GroupID] = true
		}
	}
	var out []PendingFinalization
	for gid, members := range byGroup {
		if active[gid] || s.Groups[gid].Sealed {
			continue
		}
		p := PendingFinalization{GroupID: gid, Members: members}
		if f, ok := s.Finals[gid]; ok {
			fc := f
			p.Final = &fc
		}
		out = append(out, p)
	}
	return out
}
