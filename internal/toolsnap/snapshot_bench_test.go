package toolsnap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchRepo builds a repository with n committed files and d dirty
// files for capture benchmarks.
func benchRepo(b *testing.B, n, d int) *Store {
	b.Helper()
	root := b.TempDir()
	runB(b, root, "git", "init", "-q", "-b", "main")
	runB(b, root, "git", "config", "user.email", "t@example.com")
	runB(b, root, "git", "config", "user.name", "t")
	for i := 0; i < n; i++ {
		rel := filepath.Join(fmt.Sprintf("pkg%02d", i%50), fmt.Sprintf("file%d.txt", i))
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(fmt.Sprintf("content %d\n", i)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	runB(b, root, "git", "add", ".")
	runB(b, root, "git", "commit", "-q", "-m", "init")
	for i := 0; i < d; i++ {
		rel := filepath.Join(fmt.Sprintf("pkg%02d", i%50), fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(filepath.Join(root, rel), []byte(fmt.Sprintf("dirty %d\n", i)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".semantica"), 0o755); err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	s, err := OpenStore(ctx, rc, filepath.Join(root, ".semantica"))
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func runB(b *testing.B, dir string, name string, args ...string) {
	b.Helper()
	if name != "git" {
		b.Fatalf("benchmark helper only runs git, got %q", name)
	}
	if _, err := gitOutput(context.Background(), dir, args...); err != nil {
		b.Fatalf("git %v: %v", args, err)
	}
}

func benchCapture(b *testing.B, files, dirty int) {
	s := benchRepo(b, files, dirty)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.CaptureBefore(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCaptureClean500(b *testing.B)       { benchCapture(b, 500, 0) }
func BenchmarkCaptureClean5000(b *testing.B)      { benchCapture(b, 5000, 0) }
func BenchmarkCaptureDirty500of5000(b *testing.B) { benchCapture(b, 5000, 500) }
func BenchmarkCaptureDirty10of500(b *testing.B)   { benchCapture(b, 500, 10) }
