package blobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/platform"
)

// Retain copies and verifies a required object, then syncs its file and directories.
// The destination owns a separate link, independent of source-store cleanup.
func (s *Store) Retain(ctx context.Context, hash string, src *Store) error {
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != hash {
		return fmt.Errorf("invalid blob hash %q", hash)
	}
	if !s.Exists(hash) {
		if src == nil {
			return fmt.Errorf("required blob %s has no source", hash)
		}
		if err := s.Propagate(ctx, hash, src); err != nil {
			return err
		}
	}
	raw, err := s.Get(ctx, hash)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != hash {
		return fmt.Errorf("blob %s content mismatch", hash)
	}
	f, err := os.OpenFile(s.blobPath(hash), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	_ = f.Close()
	if err != nil {
		return err
	}
	for dir := filepath.Dir(s.blobPath(hash)); ; dir = filepath.Dir(dir) {
		if err := platform.SyncDir(dir); err != nil {
			return err
		}
		if dir == filepath.Clean(s.root) {
			break
		}
	}
	return platform.SyncDir(filepath.Dir(s.root))
}
