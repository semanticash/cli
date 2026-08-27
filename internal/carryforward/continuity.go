// Package carryforward selects modified paths whose committed content matches an
// earlier workspace observation.
package carryforward

import "github.com/semanticash/cli/internal/store/blobs"

// Observation identifies a completed version-2 workspace checkpoint.
type Observation struct {
	CheckpointID     string
	Sequence         int64
	EventCursor      int64
	EventCursorValid bool
	Manifest         blobs.Manifest
}

// Anchor identifies the observation and modified paths with matching content.
type Anchor struct {
	CheckpointID string
	Sequence     int64
	EventCursor  int64
	Paths        []string
}

// SelectContinuousPaths returns modified paths whose commit CAS identity matches
// the newest workspace observation. It proves matching observed content, not an
// uninterrupted edit history. Invalid or ambiguous inputs fail closed. Symlinks
// and gitlinks are excluded.
func SelectContinuousPaths(commit blobs.Manifest, modifiedPaths []string, observations []Observation) *Anchor {
	if !commit.IsCommitScoped() {
		return nil
	}
	top, ok := newestObservation(observations)
	if !ok {
		return nil
	}
	if !validAnchorMetadata(top) || !top.Manifest.IsWorkspaceScoped() {
		return nil
	}

	commitBlobs := regularBlobIndex(commit, true)
	obsBlobs := regularBlobIndex(top.Manifest, false)

	anchor := &Anchor{CheckpointID: top.CheckpointID, Sequence: top.Sequence, EventCursor: top.EventCursor}
	seen := make(map[string]bool, len(modifiedPaths))
	for _, p := range modifiedPaths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		cb, okc := commitBlobs[p]
		ob, oko := obsBlobs[p]
		if !okc || !oko {
			continue
		}
		if cb == ob {
			anchor.Paths = append(anchor.Paths, p)
		}
	}
	if len(anchor.Paths) == 0 {
		return nil
	}
	return anchor
}

// validAnchorMetadata validates the event-window boundary.
func validAnchorMetadata(o Observation) bool {
	return o.CheckpointID != "" && o.Sequence > 0 && o.EventCursorValid && o.EventCursor >= 0
}

// newestObservation returns the sole observation with the highest sequence.
func newestObservation(obs []Observation) (Observation, bool) {
	if len(obs) == 0 {
		return Observation{}, false
	}
	top := obs[0]
	tie := false
	for _, o := range obs[1:] {
		switch {
		case o.Sequence > top.Sequence:
			top, tie = o, false
		case o.Sequence == top.Sequence:
			tie = true
		}
	}
	if tie {
		return Observation{}, false
	}
	return top, true
}

// regularBlobIndex maps regular-file paths to CAS hashes.
func regularBlobIndex(m blobs.Manifest, commitScope bool) map[string]string {
	out := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		if f.Path == "" || f.Blob == "" {
			continue
		}
		if commitScope {
			if f.EntryType != blobs.EntryRegular {
				continue
			}
		} else if f.IsSymlink {
			continue
		}
		out[f.Path] = f.Blob
	}
	return out
}
