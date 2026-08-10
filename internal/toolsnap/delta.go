package toolsnap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// DeltaVersion is the canonical tool-delta schema version.
const DeltaVersion = 1

// deltaKind identifies canonical tool-delta blobs.
const deltaKind = "agent_tool_delta"

// Delta is canonical evidence for one tool window or concurrency group.
// Equivalent values serialize identically for content addressing.
type Delta struct {
	Version  int         `json:"version"`
	Kind     string      `json:"kind"`
	Scope    string      `json:"scope"`  // "tool" or "concurrent_group"
	Status   string      `json:"status"` // "complete" or "partial"
	Reason   string      `json:"reason,omitempty"`
	Window   Window      `json:"window"`
	Actors   []Actor     `json:"actors"`
	ToolUses []ToolUse   `json:"tool_uses"`
	Files    []FileDelta `json:"files"`
	Limits   Limits      `json:"limits"`
}

type Window struct {
	StartedAt   int64 `json:"started_at"`
	CompletedAt int64 `json:"completed_at"`
	DurationMS  int64 `json:"duration_ms"`
}

type Actor struct {
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
}

type ToolUse struct {
	ToolUseID      string `json:"tool_use_id"`
	ToolName       string `json:"tool_name"`
	CommandSummary string `json:"command_summary,omitempty"`
	EventID        string `json:"event_id"`
	Actor          int    `json:"actor"`
}

// FileDelta records one file change. Modes are Git octal strings.
// Binary and truncated files carry file-level evidence without hunks.
type FileDelta struct {
	Path            string `json:"path"`
	Operation       string `json:"operation"` // create, edit, delete, typechange
	BeforeHash      string `json:"before_hash"`
	AfterHash       string `json:"after_hash"`
	BeforeMode      string `json:"before_mode"`
	AfterMode       string `json:"after_mode"`
	Binary          bool   `json:"binary,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
	OldNoEOFNewline bool   `json:"old_no_eof_newline,omitempty"`
	NewNoEOFNewline bool   `json:"new_no_eof_newline,omitempty"`
	Hunks           []Hunk `json:"hunks,omitempty"`
}

type Limits struct {
	FilesObserved int   `json:"files_observed"`
	BytesRead     int64 `json:"bytes_read"`
	Truncated     bool  `json:"truncated"`
}

// Validate rejects structurally invalid or contradictory evidence.
func (d *Delta) Validate() error {
	if d.Scope != "tool" && d.Scope != "concurrent_group" {
		return fmt.Errorf("toolsnap: unsupported delta scope %q", d.Scope)
	}
	switch d.Status {
	case "complete":
		if d.Reason != "" {
			return fmt.Errorf("toolsnap: complete delta carries reason %q", d.Reason)
		}
	case "partial":
		if d.Reason == "" {
			return fmt.Errorf("toolsnap: partial delta without a reason")
		}
	default:
		return fmt.Errorf("toolsnap: unsupported delta status %q", d.Status)
	}
	actorSeen := map[Actor]bool{}
	for _, a := range d.Actors {
		if actorSeen[a] {
			return fmt.Errorf("toolsnap: duplicate actor identity %+v", a)
		}
		actorSeen[a] = true
	}
	tuSeen := map[string]bool{}
	for _, tu := range d.ToolUses {
		if tu.Actor < 0 || tu.Actor >= len(d.Actors) {
			return fmt.Errorf("toolsnap: tool use %s references actor %d of %d", tu.ToolUseID, tu.Actor, len(d.Actors))
		}
		key := tu.ToolUseID + "\x00" + tu.EventID
		if tuSeen[key] {
			return fmt.Errorf("toolsnap: duplicate tool use %s/%s", tu.ToolUseID, tu.EventID)
		}
		tuSeen[key] = true
	}
	pathSeen := map[string]bool{}
	anyTruncated := false
	for _, f := range d.Files {
		anyTruncated = anyTruncated || f.Truncated
		if pathSeen[f.Path] {
			return fmt.Errorf("toolsnap: duplicate file path %q", f.Path)
		}
		pathSeen[f.Path] = true
		switch f.Operation {
		case "create", "edit", "delete", "typechange":
		default:
			return fmt.Errorf("toolsnap: unsupported operation %q for %q", f.Operation, f.Path)
		}
		if f.Binary && len(f.Hunks) > 0 {
			return fmt.Errorf("toolsnap: binary file %q carries hunks", f.Path)
		}
		if f.Truncated && (len(f.Hunks) > 0 || f.Binary) {
			return fmt.Errorf("toolsnap: truncated file %q carries hunks or binary marker", f.Path)
		}
		for _, h := range f.Hunks {
			if h.OldCount != len(h.OldLines) || h.NewCount != len(h.NewLines) {
				return fmt.Errorf("toolsnap: hunk counts disagree with lines in %q", f.Path)
			}
		}
	}
	// Keep aggregate and per-file truncation consistent.
	if d.Limits.Truncated != anyTruncated {
		return fmt.Errorf("toolsnap: limits.truncated %v disagrees with per-file truncation %v",
			d.Limits.Truncated, anyTruncated)
	}
	return nil
}

// validateHunkGeometry requires valid, non-adjacent canonical hunks.
func (d *Delta) validateHunkGeometry() error {
	for _, f := range d.Files {
		for i, h := range f.Hunks {
			if h.OldStart < 1 || h.NewStart < 1 {
				return fmt.Errorf("toolsnap: hunk start below 1 in %q", f.Path)
			}
			if h.OldCount == 0 && h.NewCount == 0 {
				return fmt.Errorf("toolsnap: empty hunk in %q", f.Path)
			}
			if i == 0 {
				continue
			}
			prev := f.Hunks[i-1]
			if h.OldStart <= prev.OldStart+prev.OldCount ||
				h.NewStart <= prev.NewStart+prev.NewCount {
				return fmt.Errorf("toolsnap: overlapping or adjacent hunks in %q", f.Path)
			}
		}
	}
	return nil
}

// Normalize orders repeated fields and replaces nil collections with
// empty collections.
func (d *Delta) Normalize() {
	old := d.Actors
	perm := make([]int, len(old))
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(i, j int) bool {
		a, b := old[perm[i]], old[perm[j]]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		return a.TurnID < b.TurnID
	})
	sorted := make([]Actor, len(old))
	newIndex := make([]int, len(old))
	for newPos, oldPos := range perm {
		sorted[newPos] = old[oldPos]
		newIndex[oldPos] = newPos
	}
	d.Actors = sorted
	for i := range d.ToolUses {
		d.ToolUses[i].Actor = newIndex[d.ToolUses[i].Actor]
	}

	sort.SliceStable(d.ToolUses, func(i, j int) bool {
		a, b := d.ToolUses[i], d.ToolUses[j]
		if a.Actor != b.Actor {
			return a.Actor < b.Actor
		}
		if a.ToolUseID != b.ToolUseID {
			return a.ToolUseID < b.ToolUseID
		}
		return a.EventID < b.EventID
	})
	sort.SliceStable(d.Files, func(i, j int) bool {
		return d.Files[i].Path < d.Files[j].Path
	})
	for f := range d.Files {
		hunks := d.Files[f].Hunks
		sort.SliceStable(hunks, func(i, j int) bool {
			return hunkLess(hunks[i], hunks[j])
		})
		for h := range hunks {
			if hunks[h].OldLines == nil {
				hunks[h].OldLines = []string{}
			}
			if hunks[h].NewLines == nil {
				hunks[h].NewLines = []string{}
			}
		}
	}
	if d.Actors == nil {
		d.Actors = []Actor{}
	}
	if d.ToolUses == nil {
		d.ToolUses = []ToolUse{}
	}
	if d.Files == nil {
		d.Files = []FileDelta{}
	}
}

// hunkLess orders hunks by coordinates, counts, and content.
func hunkLess(a, b Hunk) bool {
	if a.OldStart != b.OldStart {
		return a.OldStart < b.OldStart
	}
	if a.NewStart != b.NewStart {
		return a.NewStart < b.NewStart
	}
	if a.OldCount != b.OldCount {
		return a.OldCount < b.OldCount
	}
	if a.NewCount != b.NewCount {
		return a.NewCount < b.NewCount
	}
	for i := 0; i < len(a.OldLines) && i < len(b.OldLines); i++ {
		if a.OldLines[i] != b.OldLines[i] {
			return a.OldLines[i] < b.OldLines[i]
		}
	}
	for i := 0; i < len(a.NewLines) && i < len(b.NewLines); i++ {
		if a.NewLines[i] != b.NewLines[i] {
			return a.NewLines[i] < b.NewLines[i]
		}
	}
	return false
}

// CanonicalBytes validates, normalizes, and serializes the delta.
func (d *Delta) CanonicalBytes() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	d.Version = DeltaVersion
	d.Kind = deltaKind
	d.Normalize()
	if err := d.validateHunkGeometry(); err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

// ParseDelta accepts only the canonical encoding for this schema version.
func ParseDelta(raw []byte) (*Delta, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d Delta
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("toolsnap: parse delta: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("toolsnap: trailing data after delta")
	}
	if d.Version != DeltaVersion {
		return nil, fmt.Errorf("toolsnap: unsupported delta version %d", d.Version)
	}
	if d.Kind != deltaKind {
		return nil, fmt.Errorf("toolsnap: unsupported delta kind %q", d.Kind)
	}
	// Re-encoding catches non-canonical order and null collections.
	var copy Delta
	if err := json.Unmarshal(raw, &copy); err != nil {
		return nil, fmt.Errorf("toolsnap: parse delta: %w", err)
	}
	canon, err := copy.CanonicalBytes()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canon, raw) {
		return nil, fmt.Errorf("toolsnap: blob is not in canonical form")
	}
	return &d, nil
}
