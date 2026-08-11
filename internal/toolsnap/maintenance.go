package toolsnap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DefaultStaleWindowAge is the retention period for pending snapshots.
const DefaultStaleWindowAge = 24 * time.Hour

// DefaultPruneGrace protects objects for at least the pending-snapshot period.
const DefaultPruneGrace = DefaultStaleWindowAge

// maintenanceLockWait keeps maintenance from queuing behind capture.
const maintenanceLockWait = 250 * time.Millisecond

// maintenanceMaxHold bounds lock acquisition and maintenance work.
// It is a variable so timeout behavior can be tested.
var maintenanceMaxHold = time.Second

// MaintenanceReport describes one maintenance pass.
type MaintenanceReport struct {
	// Deferred reports an incomplete pass. Counters include completed work.
	Deferred      bool
	ActiveWindows int
	RefsDeleted   int
	RefsKept      int
	MarkersPruned int
	PruneRan      bool
	// StoreBytes is the store object size after the pass.
	StoreBytes int64
}

// Maintain removes stale refs and prunes expired objects from the isolated
// store. It holds the registry lock and defers while capture is active.
func (s *Store) Maintain(ctx context.Context, reg *Registry, grace time.Duration) (MaintenanceReport, error) {
	if grace < DefaultPruneGrace {
		grace = DefaultPruneGrace
	}
	report := MaintenanceReport{}

	// Leave most of the deadline for maintenance work.
	passCtx, cancel := context.WithTimeout(ctx, maintenanceMaxHold)
	defer cancel()
	lockCtx, lockCancel := context.WithTimeout(passCtx, maintenanceLockWait)
	defer lockCancel()

	_, err := reg.withLock(lockCtx, func(state *registryState) (bool, error) {
		for _, w := range state.Windows {
			if w.Status == "active" {
				report.ActiveWindows++
			}
		}
		if report.ActiveWindows > 0 {
			// Defer while capture owns the registry.
			report.Deferred = true
			return false, nil
		}

		referenced := map[string]bool{}
		protectedTrees := map[string]bool{}
		for _, w := range state.Windows {
			referenced[w.SnapshotRef] = true
			protectedTrees[w.TreeHash] = true
		}
		// Final state records tree hashes but not post-ref names.
		for _, f := range state.Finals {
			if f.PostTreeHash != "" {
				protectedTrees[f.PostTreeHash] = true
			}
		}
		refs, err := s.ListRefs(passCtx)
		if err != nil {
			return false, err
		}
		staleCutoff := time.Now().Add(-DefaultStaleWindowAge)
		for ref, target := range refs {
			// Stop large scans at the pass deadline.
			if err := passCtx.Err(); err != nil {
				return false, err
			}
			if referenced[ref] || protectedTrees[target] {
				report.RefsKept++
				continue
			}
			// Keep fresh or unreadable refs; they may still be recoverable.
			fi, err := os.Stat(filepath.Join(s.Dir, filepath.FromSlash(ref)))
			if err != nil || fi.ModTime().After(staleCutoff) {
				report.RefsKept++
				continue
			}
			// Compare-and-swap deletion preserves a ref that moved.
			if err := s.DeleteRef(passCtx, ref, target); err != nil {
				return false, err
			}
			report.RefsDeleted++
		}

		// Pending partials remain until their evidence links are durable.
		if entries, err := os.ReadDir(filepath.Join(reg.dir, "closures")); err == nil {
			for _, e := range entries {
				if err := passCtx.Err(); err != nil {
					return false, err
				}
				fi, err := e.Info()
				if err != nil || fi.ModTime().After(staleCutoff) {
					continue
				}
				if os.Remove(filepath.Join(reg.dir, "closures", e.Name())) == nil {
					report.MarkersPruned++
				}
			}
		}

		// Use an explicit grace period for unreachable objects.
		cutoff := time.Now().Add(-grace).UTC().Format(time.RFC3339)
		if maintenanceBeforePrune != nil {
			maintenanceBeforePrune()
		}
		if _, err := s.git(passCtx, "prune", "--expire", cutoff); err != nil {
			return false, fmt.Errorf("toolsnap: prune store: %w", err)
		}
		report.PruneRan = true

		// Measure a stable store while capture remains excluded.
		size, err := s.objectStoreSize(passCtx)
		if err != nil {
			return false, err
		}
		report.StoreBytes = size
		return false, nil
	})
	if err != nil {
		// Caller cancellation remains distinguishable with errors.Is.
		if ctx.Err() != nil {
			return MaintenanceReport{}, fmt.Errorf("toolsnap: maintenance cancelled (%v): %w", err, ctx.Err())
		}
		var pe *PartialError
		lockContended := errors.As(err, &pe) && pe.Reason == ReasonLockTimeout
		// A timed-out subprocess may report a signal instead of a deadline.
		passExpired := passCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded)
		if lockContended || passExpired {
			// Return completed work and defer the remainder.
			report.Deferred = true
			return report, nil
		}
		return MaintenanceReport{}, err
	}
	return report, nil
}

// maintenanceBeforePrune is a test seam for mid-pass expiry.
var maintenanceBeforePrune func()

// objectStoreSize sums the store's own object bytes. Alternate
// objects live in the user repository and are not counted. The
// traversal honors the pass bound so measurement cannot hold the
// registry lock indefinitely.
func (s *Store) objectStoreSize(ctx context.Context) (int64, error) {
	var total int64
	objects := filepath.Join(s.Dir, "objects")
	err := filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			// objects/info holds the alternates configuration, not
			// object storage.
			if d.Name() == "info" {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("toolsnap: measure store: %w", err)
	}
	return total, nil
}
