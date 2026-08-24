package blobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// oid returns a distinct SHA-1-shaped object ID.
func oid(n int) string { return fmt.Sprintf("%040x", n) }

func casOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeObjects serves content and records requested object IDs.
type fakeObjects struct {
	content   map[string][]byte
	requested []string
	skip      map[string]bool // ids the reader silently drops
}

func (f *fakeObjects) read(ctx context.Context, oids []string, fn func(string, []byte) error) error {
	for _, id := range oids {
		f.requested = append(f.requested, id)
		if f.skip[id] {
			continue
		}
		c, ok := f.content[id]
		if !ok {
			return fmt.Errorf("missing object %s", id)
		}
		if err := fn(id, c); err != nil {
			return err
		}
	}
	return nil
}

func newStore(t *testing.T) *Store {
	t.Helper()
	bs, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

func fullBuildEntries() []CommitTreeEntry {
	return []CommitTreeEntry{
		{Path: "z.txt", GitMode: "100644", GitObjectID: oid(1), GitType: "blob"},
		{Path: "a.sh", GitMode: "100755", GitObjectID: oid(2), GitType: "blob"},
		{Path: "lnk", GitMode: "120000", GitObjectID: oid(3), GitType: "blob"},
		{Path: "mod", GitMode: "160000", GitObjectID: oid(4), GitType: "commit"},
	}
}

func fullBuildContent() map[string][]byte {
	return map[string][]byte{
		oid(1): []byte("Z\n"),
		oid(2): []byte("#!/bin/sh\n"),
		oid(3): []byte("z.txt"), // symlink target bytes
	}
}

func fileByPath(m Manifest, path string) (ManifestFile, bool) {
	for _, f := range m.Files {
		if f.Path == path {
			return f, true
		}
	}
	return ManifestFile{}, false
}

func TestBuildCommitManifest_FullBuild(t *testing.T) {
	ctx := context.Background()
	bs := newStore(t)
	fg := &fakeObjects{content: fullBuildContent()}

	res, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
		Entries: fullBuildEntries(), ReadObjects: fg.read,
	})
	if err != nil {
		t.Fatalf("BuildCommitManifest: %v", err)
	}
	if !res.Manifest.IsCommitScoped() {
		t.Fatal("result is not commit-scoped")
	}
	// Entries are sorted by path.
	want := []string{"a.sh", "lnk", "mod", "z.txt"}
	for i, w := range want {
		if res.Manifest.Files[i].Path != w {
			t.Errorf("file %d = %q, want %q", i, res.Manifest.Files[i].Path, w)
		}
	}
	// Regular files and symlinks store content; gitlinks do not.
	if f, _ := fileByPath(res.Manifest, "z.txt"); f.EntryType != EntryRegular || f.Blob != casOf([]byte("Z\n")) || f.Size != 2 {
		t.Errorf("z.txt = %+v", f)
	}
	if f, _ := fileByPath(res.Manifest, "lnk"); f.EntryType != EntrySymlink || f.Blob != casOf([]byte("z.txt")) || f.LinkTarget != "" {
		t.Errorf("lnk = %+v (LinkTarget must be empty for commit scope)", f)
	}
	if f, _ := fileByPath(res.Manifest, "mod"); f.EntryType != EntryGitlink || f.Blob != "" || f.Size != 0 {
		t.Errorf("mod = %+v", f)
	}
	// Only non-gitlink objects are read; the gitlink is never requested.
	if len(fg.requested) != 3 {
		t.Errorf("requested %v, want 3 object reads", fg.requested)
	}
	for _, id := range fg.requested {
		if id == oid(4) {
			t.Error("gitlink object was requested")
		}
	}
	// The stored manifest round-trips through the strict loader.
	raw, err := bs.Get(ctx, res.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(raw); err != nil {
		t.Errorf("stored manifest fails ParseManifest: %v", err)
	}
}

func TestBuildCommitManifest_ReusesUnchangedBlobs(t *testing.T) {
	ctx := context.Background()
	bs := newStore(t)
	first, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
		Entries: fullBuildEntries(), ReadObjects: (&fakeObjects{content: fullBuildContent()}).read,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reuse should avoid all object reads.
	fg := &fakeObjects{content: map[string][]byte{}}
	second, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(102), TreeID: oid(103),
		Entries: fullBuildEntries(), ReadObjects: fg.read,
		Previous: &first.Manifest, PreviousCommitLink: first.Manifest.CommitHash,
	})
	if err != nil {
		t.Fatalf("reuse build: %v", err)
	}
	if len(fg.requested) != 0 {
		t.Errorf("requested %v, want no reads when all blobs are reused", fg.requested)
	}
	if f, _ := fileByPath(second.Manifest, "z.txt"); f.Blob != casOf([]byte("Z\n")) {
		t.Errorf("reused z.txt blob = %q", f.Blob)
	}
}

func TestBuildCommitManifest_RepopulatesAbsentBlob(t *testing.T) {
	ctx := context.Background()
	bs1 := newStore(t)
	first, err := BuildCommitManifest(ctx, bs1, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
		Entries: fullBuildEntries(), ReadObjects: (&fakeObjects{content: fullBuildContent()}).read,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh store must repopulate missing CAS blobs.
	bs2 := newStore(t)
	fg := &fakeObjects{content: fullBuildContent()}
	second, err := BuildCommitManifest(ctx, bs2, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(102), TreeID: oid(103),
		Entries: fullBuildEntries(), ReadObjects: fg.read,
		Previous: &first.Manifest, PreviousCommitLink: first.Manifest.CommitHash,
	})
	if err != nil {
		t.Fatalf("repopulate build: %v", err)
	}
	if len(fg.requested) != 3 {
		t.Errorf("requested %v, want 3 reads when the CAS lacks the blobs", fg.requested)
	}
	if f, _ := fileByPath(second.Manifest, "z.txt"); !bs2.Exists(f.Blob) {
		t.Error("z.txt blob was not repopulated into the new store")
	}
}

func TestBuildCommitManifest_ChangedObjectIsReread(t *testing.T) {
	ctx := context.Background()
	bs := newStore(t)
	first, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
		Entries: fullBuildEntries(), ReadObjects: (&fakeObjects{content: fullBuildContent()}).read,
	})
	if err != nil {
		t.Fatal(err)
	}
	// z.txt now points at a new object id; the rest are unchanged.
	entries := fullBuildEntries()
	entries[0].GitObjectID = oid(9)
	fg := &fakeObjects{content: map[string][]byte{oid(9): []byte("changed\n")}}
	second, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(102), TreeID: oid(103),
		Entries: entries, ReadObjects: fg.read,
		Previous: &first.Manifest, PreviousCommitLink: first.Manifest.CommitHash,
	})
	if err != nil {
		t.Fatalf("changed-object build: %v", err)
	}
	if len(fg.requested) != 1 || fg.requested[0] != oid(9) {
		t.Errorf("requested %v, want only the changed object", fg.requested)
	}
	if f, _ := fileByPath(second.Manifest, "z.txt"); f.Blob != casOf([]byte("changed\n")) {
		t.Errorf("z.txt blob = %q, want the changed content", f.Blob)
	}
}

func TestBuildCommitManifest_DedupesSharedObject(t *testing.T) {
	ctx := context.Background()
	bs := newStore(t)
	entries := []CommitTreeEntry{
		{Path: "a", GitMode: "100644", GitObjectID: oid(1), GitType: "blob"},
		{Path: "b", GitMode: "100644", GitObjectID: oid(1), GitType: "blob"},
	}
	fg := &fakeObjects{content: map[string][]byte{oid(1): []byte("shared")}}
	res, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
		Entries: entries, ReadObjects: fg.read,
	})
	if err != nil {
		t.Fatalf("dedup build: %v", err)
	}
	if len(fg.requested) != 1 {
		t.Errorf("requested %v, want the shared object read once", fg.requested)
	}
	fa, _ := fileByPath(res.Manifest, "a")
	fb, _ := fileByPath(res.Manifest, "b")
	if fa.Blob != casOf([]byte("shared")) || fb.Blob != fa.Blob {
		t.Errorf("shared blob mismatch: a=%q b=%q", fa.Blob, fb.Blob)
	}
}

func TestBuildCommitManifest_Errors(t *testing.T) {
	ctx := context.Background()
	base := func() CommitManifestInput {
		return CommitManifestInput{
			ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
			Entries:     fullBuildEntries(),
			ReadObjects: (&fakeObjects{content: fullBuildContent()}).read,
		}
	}
	tests := map[string]func(*CommitManifestInput){
		"bad object format":    func(in *CommitManifestInput) { in.ObjectFormat = "md5" },
		"bad commit hash":      func(in *CommitManifestInput) { in.CommitHash = "xyz" },
		"bad tree id":          func(in *CommitManifestInput) { in.TreeID = "short" },
		"unsupported mode":     func(in *CommitManifestInput) { in.Entries[0].GitMode = "040000" },
		"invalid path":         func(in *CommitManifestInput) { in.Entries[0].Path = "a\x00b" },
		"duplicate path":       func(in *CommitManifestInput) { in.Entries[1].Path = in.Entries[0].Path },
		"bad object id":        func(in *CommitManifestInput) { in.Entries[0].GitObjectID = "nope" },
		"missing git type":     func(in *CommitManifestInput) { in.Entries[0].GitType = "" },
		"wrong git type":       func(in *CommitManifestInput) { in.Entries[0].GitType = "commit" },
		"gitlink missing type": func(in *CommitManifestInput) { in.Entries[3].GitType = "" },
		"missing object": func(in *CommitManifestInput) {
			in.ReadObjects = (&fakeObjects{content: map[string][]byte{}}).read
		},
		"reader skips object": func(in *CommitManifestInput) {
			in.ReadObjects = (&fakeObjects{content: fullBuildContent(), skip: map[string]bool{oid(1): true}}).read
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			bs := newStore(t)
			in := base()
			mutate(&in)
			if _, err := BuildCommitManifest(ctx, bs, in); err == nil {
				t.Errorf("BuildCommitManifest = nil error for %q", name)
			}
		})
	}
}

func TestBuildCommitManifest_LegacyPreviousNoReuse(t *testing.T) {
	ctx := context.Background()
	bs := newStore(t)
	// Version 1 cannot seed commit reuse.
	prev := Manifest{Version: 1, CreatedAt: 1, RepoRoot: "/r", Files: []ManifestFile{
		{Path: "z.txt", Blob: casOf([]byte("Z\n")), Size: 2, Mode: 0o644},
	}}
	fg := &fakeObjects{content: fullBuildContent()}
	if _, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
		Entries: fullBuildEntries(), ReadObjects: fg.read, Previous: &prev,
	}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(fg.requested) != 3 {
		t.Errorf("requested %v, want a full build (legacy previous must not reuse)", fg.requested)
	}
}

func TestBuildCommitManifest_ReuseRequiresCommitLink(t *testing.T) {
	ctx := context.Background()
	bs := newStore(t)
	first, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
		Entries: fullBuildEntries(), ReadObjects: (&fakeObjects{content: fullBuildContent()}).read,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A missing commit link disables reuse.
	noLink := &fakeObjects{content: fullBuildContent()}
	if _, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(102), TreeID: oid(103),
		Entries: fullBuildEntries(), ReadObjects: noLink.read, Previous: &first.Manifest,
	}); err != nil {
		t.Fatal(err)
	}
	if len(noLink.requested) != 3 {
		t.Errorf("requested %v, want full build without a commit link", noLink.requested)
	}

	// A mismatched commit link disables reuse.
	badLink := &fakeObjects{content: fullBuildContent()}
	if _, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(104), TreeID: oid(105),
		Entries: fullBuildEntries(), ReadObjects: badLink.read,
		Previous: &first.Manifest, PreviousCommitLink: oid(999),
	}); err != nil {
		t.Fatal(err)
	}
	if len(badLink.requested) != 3 {
		t.Errorf("requested %v, want full build with a mismatched commit link", badLink.requested)
	}
}

func TestBuildCommitManifest_RejectsBadReader(t *testing.T) {
	ctx := context.Background()
	entries := []CommitTreeEntry{{Path: "a", GitMode: "100644", GitObjectID: oid(1), GitType: "blob"}}
	base := CommitManifestInput{ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101), Entries: entries}

	t.Run("unexpected object", func(t *testing.T) {
		bs := newStore(t)
		in := base
		in.ReadObjects = func(_ context.Context, _ []string, fn func(string, []byte) error) error {
			return fn(oid(2), []byte("orphan")) // never requested
		}
		if _, err := BuildCommitManifest(ctx, bs, in); err == nil {
			t.Error("want error for an unexpected object id")
		}
		if bs.Exists(casOf([]byte("orphan"))) {
			t.Error("an orphan blob was written to the CAS")
		}
	})

	t.Run("duplicate object", func(t *testing.T) {
		bs := newStore(t)
		in := base
		in.ReadObjects = func(_ context.Context, _ []string, fn func(string, []byte) error) error {
			if err := fn(oid(1), []byte("A")); err != nil {
				return err
			}
			return fn(oid(1), []byte("A")) // already fulfilled
		}
		if _, err := BuildCommitManifest(ctx, bs, in); err == nil {
			t.Error("want error for a duplicate object id")
		}
	})
}

func TestBuildCommitManifest_SHA256RoundTrip(t *testing.T) {
	ctx := context.Background()
	bs := newStore(t)
	h := func(n int) string { return fmt.Sprintf("%064x", n) }
	entries := []CommitTreeEntry{
		{Path: "a.txt", GitMode: "100644", GitObjectID: h(1), GitType: "blob"},
		{Path: "sub", GitMode: "160000", GitObjectID: h(2), GitType: "commit"},
	}
	fg := &fakeObjects{content: map[string][]byte{h(1): []byte("sha256\n")}}
	res, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA256, CommitHash: h(100), TreeID: h(101),
		Entries: entries, ReadObjects: fg.read,
	})
	if err != nil {
		t.Fatalf("sha256 build: %v", err)
	}
	if !res.Manifest.IsCommitScoped() || res.Manifest.ObjectFormat != ObjectFormatSHA256 {
		t.Errorf("manifest = %+v, want commit-scoped sha256", res.Manifest)
	}
	raw, err := bs.Get(ctx, res.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(raw); err != nil {
		t.Errorf("stored sha256 manifest fails ParseManifest: %v", err)
	}
}

func TestBuildCommitManifest_WorkspacePreviousNoReuse(t *testing.T) {
	ctx := context.Background()
	bs := newStore(t)
	// A workspace manifest cannot seed commit reuse.
	prev := Manifest{Version: 2, Scope: ScopeWorkspace, CreatedAt: 1, RepoRoot: "/r", Files: []ManifestFile{
		{Path: "z.txt", Blob: casOf([]byte("Z\n")), Size: 2, Mode: 0o644},
	}}
	fg := &fakeObjects{content: fullBuildContent()}
	if _, err := BuildCommitManifest(ctx, bs, CommitManifestInput{
		ObjectFormat: ObjectFormatSHA1, CommitHash: oid(100), TreeID: oid(101),
		Entries: fullBuildEntries(), ReadObjects: fg.read,
		Previous: &prev, PreviousCommitLink: prev.CommitHash,
	}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(fg.requested) != 3 {
		t.Errorf("requested %v, want a full build (workspace previous must not reuse)", fg.requested)
	}
}
