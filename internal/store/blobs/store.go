package blobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	"github.com/semanticash/cli/internal/platform"
)

type Store struct {
	root    string
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func NewStore(root string) (*Store, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("init zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("init zstd decoder: %w", err)
	}
	return &Store{root: root, encoder: enc, decoder: dec}, nil
}

// Put stores bytes in a content-addressed path and returns the sha256 hex hash.
// Hash is computed on raw (uncompressed) bytes; bytes are stored compressed.
// It is idempotent: if the blob already exists, it does nothing.
func (s *Store) Put(ctx context.Context, b []byte) (hash string, size int64, err error) {
	_ = ctx

	sum := sha256.Sum256(b)
	hash = hex.EncodeToString(sum[:])
	size = int64(len(b))

	finalPath := s.blobPath(hash)

	// Fast path: already exists
	if st, err := os.Stat(finalPath); err == nil && st.Mode().IsRegular() {
		return hash, size, nil
	}

	compressed := s.encoder.EncodeAll(b, nil)

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return "", 0, fmt.Errorf("mkdir blob dir: %w", err)
	}

	// Write to temp file in same dir, then rename atomically
	tmp, err := os.CreateTemp(filepath.Dir(finalPath), "tmp-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(compressed); err != nil {
		return "", 0, fmt.Errorf("write blob temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("fsync blob temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("close blob temp: %w", err)
	}

	// Move blob into place (safe overwrite on Windows).
	if err := platform.SafeRename(tmpName, finalPath); err != nil {
		if st, statErr := os.Stat(finalPath); statErr == nil && st.Mode().IsRegular() {
			return hash, size, nil
		}
		return "", 0, fmt.Errorf("rename temp blob: %w", err)
	}

	return hash, size, nil
}

func (s *Store) Get(ctx context.Context, hash string) ([]byte, error) {
	_ = ctx
	p := s.blobPath(hash)
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	defer func() { _ = f.Close() }()

	compressed, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read blob: %w", err)
	}

	out, err := s.decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress blob: %w", err)
	}
	return out, nil
}

// Exists reports whether the compressed blob file already exists in this store.
func (s *Store) Exists(hash string) bool {
	st, err := os.Stat(s.blobPath(hash))
	return err == nil && st.Mode().IsRegular()
}

// StoredSize returns the on-disk compressed size for a blob in this store.
func (s *Store) StoredSize(hash string) (int64, error) {
	st, err := os.Stat(s.blobPath(hash))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Propagate transfers a blob by hash from src into this store. It attempts a
// hardlink first (zero-copy, same filesystem). If the hardlink fails for any
// reason other than the destination already existing, it falls back to a raw
// copy of the compressed file - no decompress/recompress cycle.
func (s *Store) Propagate(ctx context.Context, hash string, src *Store) error {
	_ = ctx

	dstPath := s.blobPath(hash)

	// Fast path: already exists in destination.
	if st, err := os.Stat(dstPath); err == nil && st.Mode().IsRegular() {
		return nil
	}

	srcPath := src.blobPath(hash)
	if _, err := os.Stat(srcPath); err != nil {
		return fmt.Errorf("source blob missing: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("mkdir blob dir: %w", err)
	}

	// Attempt hardlink (best-effort fast path).
	if err := os.Link(srcPath, dstPath); err == nil {
		return nil
	} else if os.IsExist(err) {
		// Race with concurrent propagate - blob appeared between our Stat and Link.
		return nil
	}
	// Any other link error (EXDEV, EPERM, EACCES, unsupported FS, etc.)
	// -> fall through to raw compressed file copy.

	return s.rawCopy(srcPath, dstPath)
}

// rawCopy copies a compressed blob file using the same atomic temp+rename
// pattern as Put.
func (s *Store) rawCopy(srcPath, dstPath string) error {
	sf, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source blob: %w", err)
	}
	defer func() { _ = sf.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), "tmp-*")
	if err != nil {
		return fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, sf); err != nil {
		return fmt.Errorf("copy blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync blob copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close blob copy: %w", err)
	}

	if err := platform.SafeRename(tmpName, dstPath); err != nil {
		if st, statErr := os.Stat(dstPath); statErr == nil && st.Mode().IsRegular() {
			return nil
		}
		return fmt.Errorf("rename blob copy: %w", err)
	}

	return nil
}

func (s *Store) blobPath(hash string) string {
	// shard: aa/bb/<hash>
	if len(hash) < 4 {
		return filepath.Join(s.root, hash)
	}
	return filepath.Join(s.root, hash[:2], hash[2:4], hash)
}
