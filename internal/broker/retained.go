package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/store/blobs"
)

// DurableRoutingEnabled enables retained capture without changing route selection.
// Existing retained work remains recoverable when the flag is turned off.
func DurableRoutingEnabled() bool {
	return os.Getenv("SEMANTICA_DURABLE_ROUTING") == "1" || os.Getenv("SEMANTICA_DURABLE_ROUTING") == "true"
}

// retainedEvent freezes an event and its destinations until delivery completes.
// TokenUsageValid is stored separately because RawEvent omits it from JSON.
type retainedEvent struct {
	Version         int              `json:"version"`
	Event           RawEvent         `json:"event"`
	TokenUsageValid bool             `json:"token_usage_valid"`
	Fingerprint     string           `json:"fingerprint"`
	Destinations    []RegisteredRepo `json:"destinations"`
	Delivered       map[string]bool  `json:"delivered"`
}

func routingRoot() (string, error) {
	base, err := GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "routing"), nil
}

func routeID(eventID string) string {
	h := sha256.Sum256([]byte(eventID))
	return hex.EncodeToString(h[:])
}

func eventFingerprint(ev RawEvent) (string, error) {
	data, err := json.Marshal(struct {
		Event           RawEvent
		TokenUsageValid bool
	}{ev, ev.TokenUsageValid})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

func writeRoutingFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := platform.WriteFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	if err := platform.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return platform.SyncDir(filepath.Dir(filepath.Dir(path)))
}

func saveRetained(path string, r *retainedEvent) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeRoutingFile(path, data)
}

func readRetained(path string) (*retainedEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r retainedEvent
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	r.Event.TokenUsageValid = r.TokenUsageValid
	fingerprint, err := eventFingerprint(r.Event)
	if err != nil {
		return nil, err
	}
	if r.Version != 1 || r.Event.EventID == "" || fingerprint != r.Fingerprint || filepath.Base(path) != routeID(r.Event.EventID)+".json" {
		return nil, fmt.Errorf("invalid retained event %s", path)
	}
	seen := map[string]bool{}
	for _, d := range r.Destinations {
		if d.Path == "" || d.CanonicalPath == "" || seen[d.CanonicalPath] {
			return nil, fmt.Errorf("invalid retained destination")
		}
		seen[d.CanonicalPath] = true
	}
	for d, delivered := range r.Delivered {
		if !seen[d] || !delivered {
			return nil, fmt.Errorf("invalid delivery acknowledgement")
		}
	}
	if r.Delivered == nil {
		r.Delivered = map[string]bool{}
	}
	return &r, nil
}

// RetainEvents persists all events and required blobs before the caller advances
// its transcript offset. Existing records keep their original destination sets.
func RetainEvents(ctx context.Context, events []RawEvent, matches []RepoMatch, src *blobs.Store) ([]string, error) {
	root, err := routingRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := platform.SyncDir(filepath.Dir(root)); err != nil {
		return nil, err
	}
	destinations := map[string][]RegisteredRepo{}
	seen := map[string]map[string]bool{}
	for _, m := range matches {
		d := m.Repo
		if d.CanonicalPath == "" {
			d.CanonicalPath = CanonicalRepoPath(d.Path)
		}
		for _, ev := range m.Events {
			if seen[ev.EventID] == nil {
				seen[ev.EventID] = map[string]bool{}
			}
			if !seen[ev.EventID][d.CanonicalPath] {
				destinations[ev.EventID] = append(destinations[ev.EventID], d)
				seen[ev.EventID][d.CanonicalPath] = true
			}
		}
	}
	var ids []string
	for _, ev := range events {
		if ev.EventID == "" {
			return nil, fmt.Errorf("cannot retain event without identity")
		}
		id := routeID(ev.EventID)
		fingerprint, err := eventFingerprint(ev)
		if err != nil {
			return nil, err
		}
		err = platform.WithFileLock(ctx, filepath.Join(root, id), func() error {
			done, err := os.ReadFile(filepath.Join(root, "delivered", id+".json"))
			if err == nil {
				if string(done) != fingerprint {
					return fmt.Errorf("event %s content mismatch", ev.EventID)
				}
				return nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			path := filepath.Join(root, "pending", id+".json")
			existing, err := readRetained(path)
			if err == nil {
				if existing.Fingerprint != fingerprint {
					return fmt.Errorf("event %s content mismatch", ev.EventID)
				}
				objects, err := blobs.NewStore(filepath.Join(root, "objects", id))
				if err != nil {
					return err
				}
				for _, hash := range []string{ev.PayloadHash, ev.ProvenanceHash} {
					if hash != "" {
						if err := objects.Retain(ctx, hash, src); err != nil {
							return err
						}
					}
				}
				return nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			objects, err := blobs.NewStore(filepath.Join(root, "objects", id))
			if err != nil {
				return err
			}
			for _, hash := range []string{ev.PayloadHash, ev.ProvenanceHash} {
				if hash != "" {
					if err := objects.Retain(ctx, hash, src); err != nil {
						return fmt.Errorf("retain %s evidence: %w", ev.EventID, err)
					}
				}
			}
			r := &retainedEvent{Version: 1, Event: ev, TokenUsageValid: ev.TokenUsageValid, Fingerprint: fingerprint, Delivered: map[string]bool{}}
			r.Destinations = destinations[ev.EventID]
			sort.Slice(r.Destinations, func(i, j int) bool { return r.Destinations[i].CanonicalPath < r.Destinations[j].CanonicalPath })
			return saveRetained(path, r)
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// DeliverRetained retries frozen destinations. A scope restricts writes to one
// repository, allowing callers that already hold its worker lock to drain safely.
// Unknown destinations remain retained and are never interpreted as delivered.
func DeliverRetained(ctx context.Context, ids []string, scope string) error {
	root, err := routingRoot()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if len(id) != 64 || strings.ContainsAny(id, "/\\") {
			return fmt.Errorf("invalid retained event key")
		}
		err := platform.WithFileLock(ctx, filepath.Join(root, id), func() error {
			path := filepath.Join(root, "pending", id+".json")
			r, err := readRetained(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			objects, err := blobs.NewStore(filepath.Join(root, "objects", id))
			if err != nil {
				return err
			}
			var destinationErrors []error
			for _, d := range r.Destinations {
				if r.Delivered[d.CanonicalPath] || (scope != "" && CanonicalRepoPath(scope) != d.CanonicalPath) {
					continue
				}
				if _, err := PersistRoutedEvents(ctx, d.Path, []RawEvent{r.Event}, objects); err != nil {
					destinationErrors = append(destinationErrors, fmt.Errorf("deliver %s to %s: %w", r.Event.EventID, d.Path, err))
					continue
				}
				r.Delivered[d.CanonicalPath] = true
				if err := saveRetained(path, r); err != nil {
					return err
				}
			}
			if len(r.Destinations) > 0 && len(r.Delivered) == len(r.Destinations) {
				// Keep a compact identity receipt so replay cannot reuse an ID with new content.
				if err := writeRoutingFile(filepath.Join(root, "delivered", id+".json"), []byte(r.Fingerprint)); err != nil {
					return err
				}
				if err := os.Remove(path); err != nil {
					return err
				}
				if err := platform.SyncDir(filepath.Dir(path)); err != nil {
					return err
				}
				if err := os.RemoveAll(filepath.Join(root, "objects", id)); err != nil {
					return err
				}
			}
			return errors.Join(destinationErrors...)
		})
		if err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(failures...)
}

// DrainRetained retries pending deliveries without requiring a transcript replay.
func DrainRetained(ctx context.Context, scope string) error {
	root, err := routingRoot()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(root, "pending"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return DeliverRetained(ctx, ids, scope)
}
