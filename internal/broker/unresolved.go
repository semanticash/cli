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

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/store/blobs"
)

// UnresolvedMutation retains the original event and required objects without a destination.
type UnresolvedMutation struct {
	Version         int               `json:"version"`
	Event           RawEvent          `json:"event"`
	TokenUsageValid bool              `json:"token_usage_valid"`
	Fingerprint     string            `json:"fingerprint"`
	Objects         map[string][]byte `json:"objects"`
}

func UnresolvedMutationDir() (string, error) {
	base, err := GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "unresolved-mutations"), nil
}

func unresolvedFingerprint(ev RawEvent) string {
	// Receipt timing can change on redelivery; the first record keeps it.
	ev.Timestamp, ev.SessionStartedAt = 0, 0
	raw, _ := json.Marshal(struct {
		Event           RawEvent
		TokenUsageValid bool
	}{ev, ev.TokenUsageValid})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// RetainUnresolvedMutations must succeed before advancing a transcript offset.
// Objects are embedded so retention does not depend on the source store's lifetime.
func RetainUnresolvedMutations(ctx context.Context, events []RawEvent, src *blobs.Store) error {
	if len(events) == 0 {
		return nil
	}
	root, err := UnresolvedMutationDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := platform.SyncDir(filepath.Dir(root)); err != nil {
		return err
	}
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ev.EventID == "" {
			return fmt.Errorf("unresolved mutation has no event ID")
		}
		id := sha256.Sum256([]byte(ev.EventID))
		path := filepath.Join(root, hex.EncodeToString(id[:])+".json")
		err := platform.WithFileLock(ctx, path, func() error {
			fingerprint := unresolvedFingerprint(ev)
			raw, err := os.ReadFile(path)
			if err == nil {
				var old UnresolvedMutation
				if err := json.Unmarshal(raw, &old); err != nil {
					return err
				}
				old.Event.TokenUsageValid = old.TokenUsageValid
				if old.Version != 1 || old.Event.EventID != ev.EventID || old.Fingerprint != unresolvedFingerprint(old.Event) || old.Fingerprint != fingerprint {
					return fmt.Errorf("unresolved mutation identity/content mismatch: %s", ev.EventID)
				}
				if err := validateUnresolvedObjects(old); err != nil {
					return err
				}
				return platform.SyncDir(root)
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			record := UnresolvedMutation{Version: 1, Event: ev, TokenUsageValid: ev.TokenUsageValid, Fingerprint: fingerprint, Objects: map[string][]byte{}}
			for _, hash := range []string{ev.PayloadHash, ev.ProvenanceHash} {
				if hash == "" {
					continue
				}
				if src == nil {
					return fmt.Errorf("unresolved mutation %s: source blob store unavailable", ev.EventID)
				}
				decoded, err := hex.DecodeString(hash)
				if err != nil || len(decoded) != sha256.Size {
					return fmt.Errorf("invalid unresolved object hash")
				}
				data, err := src.Get(ctx, hash)
				if err != nil {
					return fmt.Errorf("retain unresolved object: %w", err)
				}
				record.Objects[hash] = data
			}
			if err := validateUnresolvedObjects(record); err != nil {
				return err
			}
			raw, err = json.Marshal(record)
			if err != nil {
				return err
			}
			if err := platform.WriteFileAtomic(path, raw, 0o600); err != nil {
				return err
			}
			return platform.SyncDir(root)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func validateUnresolvedObjects(r UnresolvedMutation) error {
	for _, hash := range []string{r.Event.PayloadHash, r.Event.ProvenanceHash} {
		if hash == "" {
			continue
		}
		data, ok := r.Objects[hash]
		sum := sha256.Sum256(data)
		if !ok || hex.EncodeToString(sum[:]) != hash {
			return fmt.Errorf("unresolved object missing or corrupt: %s", hash)
		}
	}
	return nil
}
