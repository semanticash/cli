package blobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPutGet_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := []byte("hello, semantica")
	hash, size, err := s.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}

	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}

	// Hash should be SHA256 of raw bytes.
	want := sha256.Sum256(data)
	wantHex := hex.EncodeToString(want[:])
	if hash != wantHex {
		t.Errorf("hash = %s, want %s", hash, wantHex)
	}

	got, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("Get returned %q, want %q", got, data)
	}
}

func TestPut_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := []byte("idempotent content")
	hash1, size1, err := s.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	hash2, size2, err := s.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}

	if hash1 != hash2 {
		t.Errorf("hashes differ: %s vs %s", hash1, hash2)
	}
	if size1 != size2 {
		t.Errorf("sizes differ: %d vs %d", size1, size2)
	}
}

func TestPutGet_EmptyBlob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	hash, size, err := s.Put(ctx, []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}

	got, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Get returned %d bytes, want 0", len(got))
	}
}

func TestPutGet_BinaryData(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}

	hash, _, err := s.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(data))
	}
	for i, b := range got {
		if b != data[i] {
			t.Errorf("byte %d: got %02x, want %02x", i, b, data[i])
			break
		}
	}
}

func TestGet_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.Get(ctx, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if err == nil {
		t.Fatal("expected error for missing blob")
	}
}

func TestBlobPath_Sharding(t *testing.T) {
	s := newTestStore(t)

	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	got := s.blobPath(hash)
	want := filepath.Join(s.root, "ab", "cd", hash)
	if got != want {
		t.Errorf("blobPath = %q, want %q", got, want)
	}
}

func TestBlobPath_ShortHash(t *testing.T) {
	s := newTestStore(t)

	// Edge case: hash shorter than 4 chars (shouldn't happen in practice).
	got := s.blobPath("abc")
	want := filepath.Join(s.root, "abc")
	if got != want {
		t.Errorf("blobPath short = %q, want %q", got, want)
	}
}

func TestPut_DifferentContentDifferentHash(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	h1, _, _ := s.Put(ctx, []byte("content A"))
	h2, _, _ := s.Put(ctx, []byte("content B"))

	if h1 == h2 {
		t.Error("different content produced same hash")
	}
}

func TestGet_CorruptedBlob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Put a valid blob to establish the shard directory and path.
	data := []byte("valid content")
	hash, _, err := s.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}

	// Overwrite the on-disk blob with invalid (non-zstd) bytes.
	blobFile := s.blobPath(hash)
	if err := os.WriteFile(blobFile, []byte("this is not valid zstd data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Get should fail with a decompression error.
	_, err = s.Get(ctx, hash)
	if err == nil {
		t.Fatal("expected error for corrupted blob")
	}
	if !strings.Contains(err.Error(), "decompress") {
		t.Errorf("expected decompress error, got: %v", err)
	}
}

func TestPutGet_LargeBlob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 1 MB of repeated data (should compress well).
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}

	hash, size, err := s.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}

	// Verify on-disk blob is actually compressed (smaller than raw).
	blobFile := s.blobPath(hash)
	fi, err := os.Stat(blobFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= int64(len(data)) {
		t.Errorf("compressed size %d >= raw size %d; compression not working", fi.Size(), len(data))
	}

	got, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("round-trip length: got %d, want %d", len(got), len(data))
	}
}

func TestPropagate_SameFilesystem(t *testing.T) {
	base := t.TempDir()
	src, err := NewStore(filepath.Join(base, "src"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := NewStore(filepath.Join(base, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	data := []byte("hello propagate")
	hash, _, err := src.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}

	if err := dst.Propagate(ctx, hash, src); err != nil {
		t.Fatal(err)
	}

	// Verify content is correct in destination.
	got, err := dst.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get from dst after Propagate: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("propagated content = %q, want %q", got, data)
	}

	// Verify hardlink: same inode on same filesystem.
	srcInfo, _ := os.Stat(src.blobPath(hash))
	dstInfo, _ := os.Stat(dst.blobPath(hash))
	if !os.SameFile(srcInfo, dstInfo) {
		t.Error("expected hardlink (same inode), got separate files")
	}
}

func TestPropagate_Idempotent(t *testing.T) {
	base := t.TempDir()
	src, err := NewStore(filepath.Join(base, "src"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := NewStore(filepath.Join(base, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	hash, _, err := src.Put(ctx, []byte("idempotent"))
	if err != nil {
		t.Fatal(err)
	}

	// Propagate twice - second call should not error.
	if err := dst.Propagate(ctx, hash, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Propagate(ctx, hash, src); err != nil {
		t.Fatalf("second Propagate should be idempotent: %v", err)
	}
}

func TestPropagate_SourceMissing(t *testing.T) {
	base := t.TempDir()
	src, err := NewStore(filepath.Join(base, "src"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := NewStore(filepath.Join(base, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	err = dst.Propagate(ctx, "deadbeef0000000000000000000000000000000000000000000000000000dead", src)
	if err == nil {
		t.Fatal("expected error for missing source blob")
	}
}

func TestPropagate_AlreadyExistsIndependently(t *testing.T) {
	base := t.TempDir()
	src, err := NewStore(filepath.Join(base, "src"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := NewStore(filepath.Join(base, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	data := []byte("exists in both")
	hash, _, err := src.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	// Put independently in dst too.
	if _, _, err := dst.Put(ctx, data); err != nil {
		t.Fatal(err)
	}

	// Propagate should succeed (blob already exists in dst).
	if err := dst.Propagate(ctx, hash, src); err != nil {
		t.Fatalf("Propagate when dst already has blob: %v", err)
	}
}
