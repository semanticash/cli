package toolsnap

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StorageInspection reports read-only registry and snapshot-store state using
// the same eligibility rules as maintenance.
type StorageInspection struct {
	// Now is the reference time. RefCutoff and ObjectExpire derive from it so all
	// age-based classifications use the same clock value.
	Now          time.Time `json:"now"`
	RefCutoff    time.Time `json:"ref_cutoff"`    // Now - DefaultStaleWindowAge (loose-ref age rule)
	ObjectExpire time.Time `json:"object_expire"` // Now - grace (unreachable-object prune rule)

	// Deferred reports that eligibility was not measured. DeferReason identifies
	// the cause. Eligibility fields remain zero so the inspection can be retried.
	Deferred    bool   `json:"deferred"`
	DeferReason string `json:"defer_reason,omitempty"`

	// Registry state. Durable evidence-link history is outside this report.
	NextSeq              int64 `json:"next_seq"` // monotonic capture counter
	Windows              int   `json:"windows"`
	ActiveWindows        int   `json:"active_windows"`
	Groups               int   `json:"groups"` // total groups, open plus sealed
	SealedGroups         int   `json:"sealed_groups"`
	PendingFinalizations int   `json:"pending_finalizations"`
	PendingPartials      int   `json:"pending_partials"`
	Tombstones           int   `json:"tombstones"`
	MalformedTombstones  int   `json:"malformed_tombstones"`
	ClosureMarkers       int   `json:"closure_markers"`

	// Ref classification, disjoint and summing to RefsTotal.
	RefsTotal           int `json:"refs_total"`
	RefsReferenced      int `json:"refs_referenced"`       // named by an open window's SnapshotRef
	RefsTargetProtected int `json:"refs_target_protected"` // target tree protected by a window or group final
	RefsFresh           int `json:"refs_fresh"`            // unprotected, loose, readable, newer than RefCutoff
	RefsUnreadable      int `json:"refs_unreadable"`       // unprotected but unreadable or packed (no loose file)
	RefsStaleEligible   int `json:"refs_stale_eligible"`   // unprotected, loose, readable, at or before RefCutoff

	EligibleObjects     int   `json:"eligible_objects"`      // unreachable loose objects older than ObjectExpire
	EligibleObjectBytes int64 `json:"eligible_object_bytes"` // on-disk bytes of those objects

	TotalObjects int64 `json:"total_objects"` // loose plus in-pack
	TotalBytes   int64 `json:"total_bytes"`   // loose plus pack bytes
	GarbageCount int64 `json:"garbage_count"`
	GarbageBytes int64 `json:"garbage_bytes"`
}

// RefsKept returns the refs a Maintain pass would keep (total minus eligible),
// mirroring MaintenanceReport.RefsKept.
func (i StorageInspection) RefsKept() int { return i.RefsTotal - i.RefsStaleEligible }

// InspectStorage measures registry and store state without modifying either.
// It uses now for all age cutoffs and holds the coordination locks while
// measuring protected snapshot state. Active work, unavailable locks, and
// concurrent lock-free recovery updates return a retryable Deferred result.
func (s *Store) InspectStorage(ctx context.Context, reg *Registry, grace time.Duration, now time.Time) (StorageInspection, error) {
	if grace < DefaultPruneGrace {
		grace = DefaultPruneGrace
	}
	insp := StorageInspection{
		Now:          now,
		RefCutoff:    now.Add(-DefaultStaleWindowAge),
		ObjectExpire: now.Add(-grace),
	}
	semDir := filepath.Dir(s.Dir)
	// A registry coordinates only the store in the same .semantica directory.
	if filepath.Dir(reg.dir) != semDir {
		return StorageInspection{}, fmt.Errorf("toolsnap: registry %q does not belong to store %q", reg.dir, s.Dir)
	}

	passCtx, cancel := context.WithTimeout(ctx, maintenanceMaxHold)
	defer cancel()
	lockCtx, lockCancel := context.WithTimeout(passCtx, maintenanceLockWait)
	defer lockCancel()

	lockErr := reg.WithCoordinationLockReadOnly(lockCtx, func() error {
		// Partial and tombstone records are lock-free. Bracket their published
		// names and defer if the entry set changes during the measurement.
		transientBefore, err := transientFingerprint(semDir)
		if err != nil {
			return err
		}
		snap, err := registrySnapshot(semDir)
		if err != nil {
			// Retry a read that raced a lock-free update.
			if after, ferr := transientFingerprint(semDir); ferr == nil && after != transientBefore {
				insp = deferredInspection(now, insp, "transient_change")
				return nil
			}
			return err
		}
		if storageInspectMidSeam != nil {
			storageInspectMidSeam()
		}
		insp.NextSeq = snap.NextSeq
		insp.Windows = len(snap.Windows)
		insp.Groups = len(snap.Groups)
		for _, g := range snap.Groups {
			if g.Sealed {
				insp.SealedGroups++
			}
		}
		insp.PendingFinalizations = len(snap.CompleteGroups())
		insp.PendingPartials = len(snap.Partials)
		insp.Tombstones = len(snap.Tombstones)
		insp.MalformedTombstones = len(snap.MalformedTombstones)
		markers, err := countRegistryFiles(filepath.Join(semDir, "tool-windows", "closures"))
		if err != nil {
			return err
		}
		insp.ClosureMarkers = markers
		for _, w := range snap.Windows {
			if w.Status == "active" {
				insp.ActiveWindows++
			}
		}
		// Maintenance also defers while a capture window is active.
		if insp.ActiveWindows > 0 {
			insp.Deferred = true
			insp.DeferReason = "active_windows"
			return nil
		}

		referenced, protectedTrees := refProtectionSets(snap.Windows, snap.Finals)
		refs, err := s.ListRefs(passCtx)
		if err != nil {
			return err
		}
		insp.RefsTotal = len(refs)
		for ref, target := range refs {
			switch {
			case referenced[ref]:
				insp.RefsReferenced++
			case protectedTrees[target]:
				insp.RefsTargetProtected++
			case refStaleEligible(s.Dir, ref, target, referenced, protectedTrees, insp.RefCutoff):
				insp.RefsStaleEligible++
			case refIsUnreadable(s.Dir, ref):
				insp.RefsUnreadable++
			default:
				insp.RefsFresh++
			}
		}

		ids, err := s.pruneEligibleObjects(passCtx, insp.ObjectExpire)
		if err != nil {
			return err
		}
		insp.EligibleObjects = len(ids)
		if insp.EligibleObjectBytes, err = s.objectDiskBytes(passCtx, ids); err != nil {
			return err
		}
		if err := s.readObjectCounts(passCtx, &insp); err != nil {
			return err
		}
		transientAfter, err := transientFingerprint(semDir)
		if err != nil {
			return err
		}
		if transientAfter != transientBefore {
			insp = deferredInspection(now, insp, "transient_change")
		}
		return nil
	})
	if lockErr != nil {
		// Parent cancellation is an error. Internal deadlines and lock contention
		// are retryable.
		if ctx.Err() != nil {
			return StorageInspection{}, fmt.Errorf("toolsnap: inspection cancelled (%v): %w", lockErr, ctx.Err())
		}
		var pe *PartialError
		if (errors.As(lockErr, &pe) && pe.Reason == ReasonLockTimeout) || passCtx.Err() != nil {
			reason := "lock_timeout"
			if passCtx.Err() != nil {
				reason = "deadline"
			}
			return deferredInspection(now, insp, reason), nil
		}
		return StorageInspection{}, lockErr
	}
	return insp, nil
}

// deferredInspection returns a Deferred result that preserves only the pinned
// timestamps, discarding any partially measured eligibility.
func deferredInspection(now time.Time, insp StorageInspection, reason string) StorageInspection {
	return StorageInspection{
		Now: now, RefCutoff: insp.RefCutoff, ObjectExpire: insp.ObjectExpire,
		Deferred: true, DeferReason: reason,
	}
}

// registrySnapshot is replaceable in tests that exercise registry read errors.
var registrySnapshot = InspectRegistry

// storageInspectMidSeam is replaceable in tests that exercise concurrent
// lock-free updates.
var storageInspectMidSeam func()

// transientFingerprint returns sorted published partial and tombstone names.
// Temporary files are ignored.
func transientFingerprint(semDir string) (string, error) {
	base := filepath.Join(semDir, "tool-windows")
	var names []string
	for _, sub := range []string{"partials", "tombstones"} {
		entries, err := os.ReadDir(filepath.Join(base, sub))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		for _, e := range entries {
			if e.IsDir() || strings.Contains(e.Name(), ".tmp-") {
				continue
			}
			names = append(names, sub+"/"+e.Name())
		}
	}
	sort.Strings(names)
	return strings.Join(names, "\n"), nil
}

// countRegistryFiles counts non-temporary entries in a registry subdirectory.
// A missing directory is zero.
func countRegistryFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		n++
	}
	return n, nil
}

// pruneEligibleObjects returns the object IDs a prune at expire would remove,
// running prune with --dry-run (read-only).
func (s *Store) pruneEligibleObjects(ctx context.Context, expire time.Time) ([]string, error) {
	out, err := s.git(ctx, "prune", "-n", "--expire", expire.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("toolsnap: dry-run prune: %w", err)
	}
	return parsePruneDryRun(out)
}

// parsePruneDryRun extracts object IDs from prune --dry-run output. Each line is
// "<sha> <type>"; a line that does not match that shape fails rather than being
// silently skipped.
func parsePruneDryRun(out string) ([]string, error) {
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] == "" {
			return nil, fmt.Errorf("toolsnap: unexpected prune line %q", line)
		}
		ids = append(ids, fields[0])
	}
	return ids, nil
}

// objectDiskBytes sums the on-disk size of the given objects in one
// batch-check pass, failing closed on any missing, malformed, duplicate, or
// unmatched response.
func (s *Store) objectDiskBytes(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	stdin := []byte(strings.Join(ids, "\n") + "\n")
	out, err := s.gitStdin(ctx, nil, stdin, "cat-file", "--batch-check=%(objectname) %(objectsize:disk)")
	if err != nil {
		return 0, fmt.Errorf("toolsnap: object sizes: %w", err)
	}
	return parseObjectSizes(out, ids)
}

// parseObjectSizes requires exactly one valid, non-negative size response per
// requested object. Missing, malformed, duplicate, extra, or unmatched
// responses fail the inspection rather than underreport bytes.
func parseObjectSizes(out string, ids []string) (int64, error) {
	sizes := make(map[string]int64, len(ids))
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		oid, rest, ok := strings.Cut(line, " ")
		if !ok {
			return 0, fmt.Errorf("toolsnap: malformed cat-file line %q", line)
		}
		if rest == "missing" {
			return 0, fmt.Errorf("toolsnap: object %s missing during inspection", oid)
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if perr != nil || n < 0 {
			return 0, fmt.Errorf("toolsnap: bad size for %s: %q", oid, rest)
		}
		if _, dup := sizes[oid]; dup {
			return 0, fmt.Errorf("toolsnap: duplicate size response for %s", oid)
		}
		sizes[oid] = n
	}
	if len(sizes) != len(ids) {
		return 0, fmt.Errorf("toolsnap: %d size responses for %d requested objects", len(sizes), len(ids))
	}
	var total int64
	for _, id := range ids {
		n, ok := sizes[id]
		if !ok {
			return 0, fmt.Errorf("toolsnap: no size response for requested object %s", id)
		}
		total += n
	}
	return total, nil
}

// objectCounts holds the aggregate store object measurement.
type objectCounts struct {
	TotalObjects int64
	TotalBytes   int64
	GarbageCount int64
	GarbageBytes int64
}

// readObjectCounts fills the aggregate object fields from count-objects -v.
func (s *Store) readObjectCounts(ctx context.Context, insp *StorageInspection) error {
	out, err := s.git(ctx, "count-objects", "-v")
	if err != nil {
		return fmt.Errorf("toolsnap: count objects: %w", err)
	}
	oc, err := parseCountObjects(out)
	if err != nil {
		return err
	}
	insp.TotalObjects = oc.TotalObjects
	insp.TotalBytes = oc.TotalBytes
	insp.GarbageCount = oc.GarbageCount
	insp.GarbageBytes = oc.GarbageBytes
	return nil
}

// parseCountObjects reads count-objects -v output, requiring every field it
// uses exactly once with a non-negative value. Sizes are reported in KiB and
// converted to bytes. Missing, duplicate, malformed, or negative fields fail.
func parseCountObjects(out string) (objectCounts, error) {
	needed := map[string]bool{
		"count": false, "size": false, "in-pack": false,
		"size-pack": false, "garbage": false, "size-garbage": false,
	}
	vals := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		seen, want := needed[k]
		if !want {
			continue
		}
		if seen {
			return objectCounts{}, fmt.Errorf("toolsnap: duplicate count-objects field %q", k)
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if perr != nil || n < 0 {
			return objectCounts{}, fmt.Errorf("toolsnap: bad count-objects field %q: %q", k, v)
		}
		needed[k] = true
		vals[k] = n
	}
	for k, seen := range needed {
		if !seen {
			return objectCounts{}, fmt.Errorf("toolsnap: missing count-objects field %q", k)
		}
	}
	totalObjects, err := checkedCountObjectsAdd("objects", vals["count"], vals["in-pack"])
	if err != nil {
		return objectCounts{}, err
	}
	totalKiB, err := checkedCountObjectsAdd("bytes", vals["size"], vals["size-pack"])
	if err != nil {
		return objectCounts{}, err
	}
	totalBytes, err := countObjectsKiBToBytes("bytes", totalKiB)
	if err != nil {
		return objectCounts{}, err
	}
	garbageBytes, err := countObjectsKiBToBytes("garbage bytes", vals["size-garbage"])
	if err != nil {
		return objectCounts{}, err
	}
	return objectCounts{
		TotalObjects: totalObjects,
		TotalBytes:   totalBytes,
		GarbageCount: vals["garbage"],
		GarbageBytes: garbageBytes,
	}, nil
}

func checkedCountObjectsAdd(name string, a, b int64) (int64, error) {
	if a > math.MaxInt64-b {
		return 0, fmt.Errorf("toolsnap: count-objects %s overflow", name)
	}
	return a + b, nil
}

func countObjectsKiBToBytes(name string, kib int64) (int64, error) {
	if kib > math.MaxInt64/1024 {
		return 0, fmt.Errorf("toolsnap: count-objects %s overflow", name)
	}
	return kib * 1024, nil
}
