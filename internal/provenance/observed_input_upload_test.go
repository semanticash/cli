package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/observedinput"
	"github.com/semanticash/cli/internal/store/blobs"
)

// synthStore stores one text observation per content slice and returns the document hash.
func synthStore(t *testing.T, contents [][]byte) (*blobs.Store, string) {
	t.Helper()
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ev := observedinput.Evidence{Version: 1, Provider: "claude-code", SessionID: "s", TurnID: "t"}
	for i, c := range contents {
		h, _, err := bs.Put(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		ev.Observations = append(ev.Observations, observedinput.ObservedInput{
			DeliveryID:  fmt.Sprintf("O%d", i),
			Provider:    "claude-code",
			Acquisition: observedinput.AcquisitionAttachment,
			Scope:       observedinput.ScopeObservedContext,
			Representation: observedinput.Representation{
				State: observedinput.RepPresent, ContentRef: h,
				ContentSize: int64(len(c)), MediaType: "text/plain",
			},
		})
	}
	doc, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	evHash, _, err := bs.Put(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	return bs, evHash
}

// loadFixtureStore loads fixture objects into CAS and rebases file locators onto repoRoot.
func loadFixtureStore(t *testing.T, repoRoot string) (*blobs.Store, string) {
	t.Helper()
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	contentDir := filepath.Join(observedInputFixtureDir, "local", "content")
	entries, err := os.ReadDir(contentDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(contentDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if h, _, err := bs.Put(ctx, b); err != nil || h != e.Name() {
			t.Fatalf("put content %s: hash %s err %v", e.Name(), h, err)
		}
	}
	docBytes, err := os.ReadFile(filepath.Join(observedInputFixtureDir, "local", "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Use a real repository for ignore checks; outbound paths remain unchanged.
	var ev observedinput.Evidence
	if err := json.Unmarshal(docBytes, &ev); err != nil {
		t.Fatal(err)
	}
	for i := range ev.Observations {
		is := &ev.Observations[i].InputSource
		if is.Kind == "file" && strings.HasPrefix(is.Locator, "/repo/") {
			is.Locator = repoRoot + strings.TrimPrefix(is.Locator, "/repo")
		}
	}
	rebased, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	evHash, _, err := bs.Put(ctx, rebased)
	if err != nil {
		t.Fatal(err)
	}
	return bs, evHash
}

// The transform must reproduce the golden document and content objects exactly.
func TestBuildObservedInputUpload_MatchesGolden(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	gitInitRepo(t, repo)
	bs, evHash := loadFixtureStore(t, repo)

	up, err := buildObservedInputUpload(ctx, bs, evHash, repo)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	wantDoc, err := os.ReadFile(filepath.Join(observedInputFixtureDir, "outbound", "observed_input.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(up.DocBytes, wantDoc) {
		t.Fatalf("outbound document not byte-identical to golden\n got: %s\nwant: %s", up.DocBytes, wantDoc)
	}
	if up.DocHash != sha256Hex(wantDoc) {
		t.Fatalf("doc hash %s, want %s", up.DocHash, sha256Hex(wantDoc))
	}

	// The produced content objects must equal the frozen outbound content set.
	wantDir := filepath.Join(observedInputFixtureDir, "outbound", "content")
	entries, err := os.ReadDir(wantDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(wantDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		want[e.Name()] = b
	}
	if len(up.Content) != len(want) {
		t.Fatalf("produced %d content objects, want %d", len(up.Content), len(want))
	}
	for h, b := range want {
		got, ok := up.Content[h]
		if !ok {
			t.Fatalf("missing outbound content object %s", h)
		}
		if !bytes.Equal(got, b) {
			t.Fatalf("content object %s bytes differ", h)
		}
		if hashHex(got) != h {
			t.Fatalf("content object %s is not content-addressed", h)
		}
	}
}

// Missing required content must prevent upload.
func TestBuildObservedInputUpload_MissingContentFails(t *testing.T) {
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Store only the evidence document, none of its content objects.
	docBytes, err := os.ReadFile(filepath.Join(observedInputFixtureDir, "local", "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	evHash, _, err := bs.Put(ctx, docBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildObservedInputUpload(ctx, bs, evHash, "/repo"); err == nil {
		t.Fatal("expected error when required content object is missing")
	}
}

// Each upload limit accepts exactly at its boundary and rejects one past it.
func TestBuildObservedInputUpload_LimitBoundaries(t *testing.T) {
	ctx := context.Background()

	t.Run("content object size", func(t *testing.T) {
		defer restoreInt(&observedInputMaxContentBytes, observedInputMaxContentBytes)
		observedInputMaxContentBytes = 8
		bsOK, evOK := synthStore(t, [][]byte{bytes.Repeat([]byte("x"), 8)})
		if _, err := buildObservedInputUpload(ctx, bsOK, evOK, "/repo"); err != nil {
			t.Fatalf("content at limit rejected: %v", err)
		}
		bs, ev := synthStore(t, [][]byte{bytes.Repeat([]byte("x"), 9)})
		if _, err := buildObservedInputUpload(ctx, bs, ev, "/repo"); err == nil {
			t.Fatal("content over limit accepted")
		}
	})

	t.Run("document size", func(t *testing.T) {
		defer restoreInt(&observedInputMaxDocumentBytes, observedInputMaxDocumentBytes)
		bs, ev := synthStore(t, [][]byte{[]byte("hello")})
		// Measure the produced outbound document, then pin the limit to it.
		up, err := buildObservedInputUpload(ctx, bs, ev, "/repo")
		if err != nil {
			t.Fatal(err)
		}
		observedInputMaxDocumentBytes = len(up.DocBytes)
		if _, err := buildObservedInputUpload(ctx, bs, ev, "/repo"); err != nil {
			t.Fatalf("document at limit rejected: %v", err)
		}
		observedInputMaxDocumentBytes = len(up.DocBytes) - 1
		if _, err := buildObservedInputUpload(ctx, bs, ev, "/repo"); err == nil {
			t.Fatal("outbound document over limit accepted")
		}
	})

	t.Run("object count", func(t *testing.T) {
		defer restoreInt(&observedInputMaxContentObjects, observedInputMaxContentObjects)
		observedInputMaxContentObjects = 3
		atLimit := [][]byte{[]byte("d-0"), []byte("d-1"), []byte("d-2")}
		bsOK, evOK := synthStore(t, atLimit)
		if _, err := buildObservedInputUpload(ctx, bsOK, evOK, "/repo"); err != nil {
			t.Fatalf("object count at limit rejected: %v", err)
		}
		bs, ev := synthStore(t, append(atLimit, []byte("d-3")))
		if _, err := buildObservedInputUpload(ctx, bs, ev, "/repo"); err == nil {
			t.Fatal("too many content objects accepted")
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		defer restoreInt(&observedInputMaxAggregateBytes, observedInputMaxAggregateBytes)
		observedInputMaxAggregateBytes = 12
		bsOK, evOK := synthStore(t, [][]byte{[]byte("aaaaaa"), []byte("bbbbbb")}) // 6 + 6 == 12
		if _, err := buildObservedInputUpload(ctx, bsOK, evOK, "/repo"); err != nil {
			t.Fatalf("aggregate at limit rejected: %v", err)
		}
		bs, ev := synthStore(t, [][]byte{[]byte("aaaaaa"), []byte("bbbbbbb")}) // 6 + 7 > 12
		if _, err := buildObservedInputUpload(ctx, bs, ev, "/repo"); err == nil {
			t.Fatal("aggregate over limit accepted")
		}
	})
}

func restoreInt(p *int, v int) { *p = v }

// storeEvidence marshals an evidence document into the store and returns its hash.
func storeEvidence(t *testing.T, bs *blobs.Store, ev observedinput.Evidence) string {
	t.Helper()
	doc, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := bs.Put(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Source metadata must omit malformed URLs, detected secrets, and private directories.
func TestBuildObservedInputUpload_MetadataNoLeak(t *testing.T) {
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("delivered summary text\n")
	ch, _, err := bs.Put(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	textRep := func(ref string) observedinput.Representation {
		return observedinput.Representation{State: observedinput.RepPresent, ContentRef: ref, ContentSize: int64(len(body)), MediaType: "text/plain"}
	}
	ev := observedinput.Evidence{
		Version: 1, Provider: "claude-code", SessionID: "s", TurnID: "t",
		Observations: []observedinput.ObservedInput{
			{DeliveryID: "u1", Provider: "claude-code", Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext,
				Representation: textRep(ch),
				InputSource:    observedinput.InputSource{Kind: "url", Locator: "https://user:p%zzword@h.example.com/a?token=SECRETQV#frag"}},
			{DeliveryID: "u2", Provider: "claude-code", Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext,
				Representation: textRep(ch),
				InputSource:    observedinput.InputSource{Kind: "url", Locator: "https://h.example.com/ghp_0123456789abcdef0123456789abcdef0123"}},
		},
		Gaps: []observedinput.Gap{{Reason: observedinput.GapFailure, Detail: "failed reading /Users/dev/private/secret.txt during scan"}},
	}
	evHash := storeEvidence(t, bs, ev)

	up, err := buildObservedInputUpload(ctx, bs, evHash, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"p%zzword", "SECRETQV", "ghp_0123456789abcdef0123456789abcdef0123", "/Users/dev"} {
		if bytes.Contains(up.DocBytes, []byte(forbidden)) {
			t.Fatalf("outbound metadata leaks %q: %s", forbidden, up.DocBytes)
		}
	}
	if !bytes.Contains(up.DocBytes, []byte("secret.txt")) {
		t.Fatal("gap detail lost the sanitized basename")
	}
}

// Mixed-source batches must withhold both ignored and outside-repo files.
func TestBuildObservedInputUpload_OutsideRepoDoesNotDisableIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	repo := t.TempDir()
	gitInitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("private.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ignoredBody := []byte("IGNORED private contents\n")
	outsideBody := []byte("OUTSIDE repo contents\n")
	hIgnored, _, _ := bs.Put(ctx, ignoredBody)
	hOutside, _, _ := bs.Put(ctx, outsideBody)
	textRep := func(ref string, n int) observedinput.Representation {
		return observedinput.Representation{State: observedinput.RepPresent, ContentRef: ref, ContentSize: int64(n), MediaType: "text/plain"}
	}
	ev := observedinput.Evidence{
		Version: 1, Provider: "claude-code", SessionID: "s", TurnID: "t",
		Observations: []observedinput.ObservedInput{
			{DeliveryID: "ign", Provider: "claude-code", Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext,
				Representation: textRep(hIgnored, len(ignoredBody)),
				InputSource:    observedinput.InputSource{Kind: "file", Locator: filepath.Join(repo, "private.txt")}},
			{DeliveryID: "out", Provider: "claude-code", Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext,
				Representation: textRep(hOutside, len(outsideBody)),
				InputSource:    observedinput.InputSource{Kind: "file", Locator: "/somewhere/outside/other.txt"}},
		},
	}
	evHash := storeEvidence(t, bs, ev)
	up, err := buildObservedInputUpload(ctx, bs, evHash, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{hIgnored, hOutside} {
		if _, ok := up.Content[h]; ok {
			t.Fatalf("content %s was uploaded despite ignore/outside restriction", shortHash(h))
		}
	}
	if bytes.Contains(up.DocBytes, ignoredBody) || bytes.Contains(up.DocBytes, outsideBody) {
		t.Fatal("restricted content leaked into the document")
	}
	var out observedinput.Evidence
	if err := json.Unmarshal(up.DocBytes, &out); err != nil {
		t.Fatal(err)
	}
	for _, o := range out.Observations {
		if o.Representation.Upload == nil || o.Representation.Upload.State != "withheld" {
			t.Fatalf("%s not withheld: %+v", o.DeliveryID, o.Representation)
		}
	}
}

// Probe failures must withhold content or return an error.
func TestBuildObservedInputUpload_ProbeFailureWithholds(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	gitInitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("private.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := func(t *testing.T) (*blobs.Store, string, []byte) {
		bs, err := blobs.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		body := []byte("IGNORED private contents\n")
		h, _, _ := bs.Put(context.Background(), body)
		ev := observedinput.Evidence{
			Version: 1, Provider: "claude-code", SessionID: "s", TurnID: "t",
			Observations: []observedinput.ObservedInput{{
				DeliveryID: "ign", Provider: "claude-code", Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext,
				Representation: observedinput.Representation{State: observedinput.RepPresent, ContentRef: h, ContentSize: int64(len(body)), MediaType: "text/plain"},
				InputSource:    observedinput.InputSource{Kind: "file", Locator: filepath.Join(repo, "private.txt")},
			}},
		}
		return bs, storeEvidence(t, bs, ev), body
	}

	t.Run("git unavailable", func(t *testing.T) {
		bs, evHash, body := build(t)
		t.Setenv("PATH", "") // git cannot be found during the probe
		up, err := buildObservedInputUpload(context.Background(), bs, evHash, repo)
		if err != nil {
			return // Errors also prevent upload.
		}
		if bytes.Contains(up.DocBytes, body) || len(up.Content) != 0 {
			t.Fatal("ignored content uploaded when git probe was unavailable")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		bs, evHash, body := build(t)
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		up, err := buildObservedInputUpload(cctx, bs, evHash, repo)
		if err != nil {
			return // Errors also prevent upload.
		}
		if bytes.Contains(up.DocBytes, body) || len(up.Content) != 0 {
			t.Fatal("ignored content uploaded when probe context was cancelled")
		}
	})
}

// Gap details must remove private directories from POSIX and Windows paths.
func TestBuildObservedInputUpload_GapPathFormats(t *testing.T) {
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ev := observedinput.Evidence{
		Version: 1, Provider: "claude-code", SessionID: "s", TurnID: "t",
		Gaps: []observedinput.Gap{
			{Reason: observedinput.GapFailure, Detail: `failed reading "/Users/private-user/project/private.txt"`},
			{Reason: observedinput.GapFailure, Detail: `failed reading (/Users/private-user/project/secret.txt)`},
			{Reason: observedinput.GapFailure, Detail: `failed reading C:\Users\private-user\project\win.txt`},
			{Reason: observedinput.GapFailure, Detail: `failed reading "/Users/Private User/Confidential Project/spec.md"`},
		},
	}
	evHash := storeEvidence(t, bs, ev)
	up, err := buildObservedInputUpload(ctx, bs, evHash, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"private-user", "/Users/", `C:\Users`, "project", "Private User", "Confidential Project"} {
		if bytes.Contains(up.DocBytes, []byte(leak)) {
			t.Fatalf("gap detail leaked %q: %s", leak, up.DocBytes)
		}
	}
	for _, keep := range []string{"private.txt", "secret.txt", "win.txt", "spec.md"} {
		if !bytes.Contains(up.DocBytes, []byte(keep)) {
			t.Fatalf("gap detail lost basename %q", keep)
		}
	}
}

// Missing local objects must fail validation even when their bytes would be withheld.
func TestBuildObservedInputUpload_WithheldVerifiesLocalObjects(t *testing.T) {
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := "1111111111111111111111111111111111111111111111111111111111111111"
	ev := observedinput.Evidence{
		Version: 1, Provider: "claude-code", SessionID: "s", TurnID: "t",
		Observations: []observedinput.ObservedInput{{
			DeliveryID: "pdf", Provider: "claude-code", Acquisition: observedinput.AcquisitionAttachment, Scope: observedinput.ScopeRequestEnvelope,
			Representation: observedinput.Representation{State: observedinput.RepPresent, ContentRef: missing, ContentSize: 100, MediaType: "application/pdf"},
			InputSource:    observedinput.InputSource{Kind: "file", Locator: "/x/y.pdf"},
		}},
	}
	evHash := storeEvidence(t, bs, ev)
	if _, err := buildObservedInputUpload(ctx, bs, evHash, "/repo"); err == nil {
		t.Fatal("withheld content with a missing local object was accepted")
	}
}

// A missing evidence document must prevent upload.
func TestBuildObservedInputUpload_CorruptDocumentFails(t *testing.T) {
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildObservedInputUpload(ctx, bs, "0000000000000000000000000000000000000000000000000000000000000000", "/repo"); err == nil {
		t.Fatal("expected error for missing/corrupt document")
	}
}
