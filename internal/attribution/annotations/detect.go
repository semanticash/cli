package annotations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// minReworkOverlap is the number of substantive lines a later edit's
// old_string must share with an earlier edit's authored content before the
// pair is treated as rework. It is deliberately above one so that a single
// incidental shared line ("same file touched twice") never triggers an
// annotation; the contract requires structural overlap.
const minReworkOverlap = 3

// Detect derives conservative timeline annotations for one commit from its
// attribution-window events and parsed diff. It emits only possible_rework
// and attempted_removed; every other observed shape is left unclassified.
//
// The result is deterministic: annotations are keyed by (kind, commit, file)
// and returned in a stable order, so re-running the detector on the same
// input yields identical IDs and ordering (idempotent server-side upsert).
func Detect(in DetectInput) []Annotation {
	projected := projectEvents(in)

	var out []Annotation
	out = append(out, detectRework(in, projected)...)
	out = append(out, detectAttemptedRemoved(in, projected)...)

	// Status depends on whether cited step refs can be resolved later.
	for i := range out {
		resolveStatus(&out[i])
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].FilePath < out[j].FilePath
	})
	return out
}

// detectRework finds files the agent authored and later revised in another
// turn. Only files present in the commit are considered, so the annotation is
// tied to landed work rather than abandoned scratch edits.
//
// The overlap signal requires replaced content. Two sources supply it: a
// Claude-shaped Edit payload old_string, and Codex apply_patch provenance
// old_text. Provider file-touch events and content-only writes carry no
// replaced content and are left unclassified.
func detectRework(in DetectInput, projected []eventEdits) []Annotation {
	var out []Annotation

	// Use stream position as the ordering tie-breaker after (ts, event_id).
	type authored struct {
		ev    Event
		seq   int
		lines map[string]struct{}
	}
	authoredByFile := map[string][]authored{}
	for i, pe := range projected {
		for path, fe := range pe.edits {
			if !in.Commit.Files[path] {
				continue
			}
			if len(fe.added) > 0 {
				authoredByFile[path] = append(authoredByFile[path], authored{ev: pe.ev, seq: i, lines: fe.added})
			}
		}
	}

	for i, pe := range projected {
		for path, fe := range pe.edits {
			if !in.Commit.Files[path] || len(fe.replaced) == 0 {
				continue
			}
			later := pe.ev
			for _, earlier := range authoredByFile[path] {
				// Same-turn edits are ordinary multi-step authoring. Rework
				// requires a distinct turn that appears earlier in the stream.
				if earlier.ev.TurnID == later.TurnID || earlier.seq >= i {
					continue
				}
				if countOverlap(fe.replaced, earlier.lines) < minReworkOverlap {
					continue
				}
				out = appendReworkAnnotation(out, in, path, earlier.ev, later)
			}
		}
	}

	return out
}

// appendReworkAnnotation adds or merges a possible_rework annotation for a
// file. At most one annotation exists per file; repeated overlaps merge their
// turns and step refs and widen the time window.
func appendReworkAnnotation(out []Annotation, in DetectInput, path string, earlier, later Event) []Annotation {
	id := annotationID(KindPossibleRework, in.CommitSHA, path)
	for i := range out {
		if out[i].ID == id {
			mergeEvidence(&out[i], in.CommitSHA, path, earlier, later)
			return out
		}
	}
	ann := Annotation{
		ID:               id,
		Kind:             KindPossibleRework,
		FilePath:         path,
		TurnIDs:          nil,
		Summary:          fmt.Sprintf("Agent revised its earlier edits to %s before the commit landed.", path),
		Confidence:       0.7,
		AlgorithmVersion: AlgorithmVersion,
	}
	mergeEvidence(&ann, in.CommitSHA, path, earlier, later)
	return append(out, ann)
}

// detectAttemptedRemoved finds files the agent authored or touched that were
// then removed, either by the commit diff (strong) or by a recognized rm
// command later in the window (weaker). A file the agent never touched is
// never annotated, so a plain human or build-cleanup deletion is ignored.
func detectAttemptedRemoved(in DetectInput, projected []eventEdits) []Annotation {
	// Earliest agent touch per file (the authoring/edit evidence).
	firstTouch := map[string]Event{}
	for _, pe := range projected {
		for path := range pe.touched {
			if cur, ok := firstTouch[path]; !ok || pe.ev.TS < cur.TS {
				firstTouch[path] = pe.ev
			}
		}
	}

	// Collect removal evidence before emitting so a file removed with rm and
	// deleted by the commit keeps both evidence anchors.
	type removal struct {
		commitDeleted bool
		rmEvents      []Event
	}
	rem := map[string]*removal{}
	get := func(path string) *removal {
		r := rem[path]
		if r == nil {
			r = &removal{}
			rem[path] = r
		}
		return r
	}

	// Strong: the commit itself deletes a file the agent had touched.
	for path := range in.Commit.FilesDeleted {
		if _, ok := firstTouch[path]; ok {
			get(path).commitDeleted = true
		}
	}

	// Weaker: a recognized rm command removed a file the agent had touched,
	// at or after the touch (a removal before the touch is unrelated).
	for _, pe := range projected {
		for path := range pe.removed {
			touch, ok := firstTouch[path]
			if !ok || pe.ev.TS < touch.TS {
				continue
			}
			r := get(path)
			r.rmEvents = append(r.rmEvents, pe.ev)
		}
	}

	paths := make([]string, 0, len(rem))
	for path := range rem {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var out []Annotation
	for _, path := range paths {
		r := rem[path]
		touch := firstTouch[path]

		confidence, summary := 0.6, fmt.Sprintf("Agent touched %s and then removed it before the commit.", path)
		if r.commitDeleted {
			confidence = 0.85
			summary = fmt.Sprintf("Agent touched %s, which the commit then deleted.", path)
		}

		ann := Annotation{
			ID:               annotationID(KindAttemptedRemoved, in.CommitSHA, path),
			Kind:             KindAttemptedRemoved,
			FilePath:         path,
			Summary:          summary,
			Confidence:       confidence,
			AlgorithmVersion: AlgorithmVersion,
			StartedAt:        touch.TS,
			EndedAt:          touch.TS,
		}
		addTurn(&ann, touch.TurnID)
		addAnchor(&ann, Anchor{EventID: touch.EventID, TurnID: touch.TurnID, FilePath: path})
		addStepRef(&ann, touch)
		for _, rmEv := range r.rmEvents {
			addTurn(&ann, rmEv.TurnID)
			addAnchor(&ann, Anchor{EventID: rmEv.EventID, TurnID: rmEv.TurnID, FilePath: path})
			addStepRef(&ann, rmEv)
			widenWindow(&ann, rmEv.TS)
		}
		addAnchor(&ann, Anchor{CommitSHA: in.CommitSHA, FilePath: path})
		out = append(out, ann)
	}

	return out
}

// resolveStatus marks an annotation complete only when every supporting step
// ref has the turn_id and event_id needed for turn-detail resolution.
func resolveStatus(ann *Annotation) {
	if len(ann.SupportingStepRefs) == 0 {
		ann.Status = StatusPartial
		return
	}
	for _, ref := range ann.SupportingStepRefs {
		if ref.TurnID == "" || ref.EventID == "" {
			ann.Status = StatusPartial
			return
		}
	}
	ann.Status = StatusComplete
}

// mergeEvidence records the authoring and revising events on a rework
// annotation, keeping turns, anchors, step refs, and the time window merged.
func mergeEvidence(ann *Annotation, commitSHA, path string, earlier, later Event) {
	addTurn(ann, earlier.TurnID)
	addTurn(ann, later.TurnID)
	addAnchor(ann, Anchor{EventID: earlier.EventID, TurnID: earlier.TurnID, FilePath: path})
	addAnchor(ann, Anchor{EventID: later.EventID, TurnID: later.TurnID, FilePath: path})
	addAnchor(ann, Anchor{CommitSHA: commitSHA, FilePath: path})
	addStepRef(ann, earlier)
	addStepRef(ann, later)
	widenWindow(ann, earlier.TS)
	widenWindow(ann, later.TS)
}

func widenWindow(ann *Annotation, ts int64) {
	if ts == 0 {
		return
	}
	if ann.StartedAt == 0 || ts < ann.StartedAt {
		ann.StartedAt = ts
	}
	if ts > ann.EndedAt {
		ann.EndedAt = ts
	}
}

func addTurn(ann *Annotation, turnID string) {
	if turnID == "" {
		return
	}
	for _, t := range ann.TurnIDs {
		if t == turnID {
			return
		}
	}
	ann.TurnIDs = append(ann.TurnIDs, turnID)
}

func addAnchor(ann *Annotation, a Anchor) {
	for _, existing := range ann.Anchors {
		if existing == a {
			return
		}
	}
	ann.Anchors = append(ann.Anchors, a)
}

func addStepRef(ann *Annotation, ev Event) {
	if ev.EventID == "" {
		return
	}
	ref := StepRef{TurnID: ev.TurnID, EventID: ev.EventID, ProvenanceHash: ev.ProvenanceHash}
	for _, existing := range ann.SupportingStepRefs {
		if existing == ref {
			return
		}
	}
	ann.SupportingStepRefs = append(ann.SupportingStepRefs, ref)
}

// countOverlap returns how many substantive lines two sets share.
func countOverlap(a, b map[string]struct{}) int {
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	n := 0
	for line := range small {
		if _, ok := large[line]; ok {
			n++
		}
	}
	return n
}

// annotationID is a stable, idempotent identifier keyed by kind, commit, and
// file. Turn membership is intentionally excluded so re-running the detector
// on a shifted window yields the same ID for the same finding.
func annotationID(kind Kind, commitSHA, path string) string {
	sum := sha256.Sum256([]byte(string(kind) + "|" + commitSHA + "|" + path))
	return fmt.Sprintf("ann_%s_%s", kind, hex.EncodeToString(sum[:])[:12])
}
