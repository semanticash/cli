package claude

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// findSampleTranscript returns a real sample-repo Claude transcript that contains
// observed-input material, or "" when none is present on this machine.
func findSampleTranscript(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, name := range []string{
		"-Users-spei-Projects-semantica-sample2",
		"-Users-spei-Projects-semantica-sample",
	} {
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", name, "*.jsonl"))
		var best string
		bestLines := 1 << 30
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			if !bytes.Contains(data, []byte(`"attachment"`)) || !bytes.Contains(data, []byte(`"type":"user"`)) {
				continue
			}
			if n := bytes.Count(data, []byte("\n")); n < bestLines {
				best, bestLines = m, n
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

// The provider normalizes a real sample-repo transcript into observed-input
// evidence without error, surfacing real requests, attachments, and content.
func TestObservedInput_RealSampleTranscriptNormalizes(t *testing.T) {
	src := findSampleTranscript(t)
	if src == "" {
		t.Skip("no real sample Claude transcript with attachments on this machine")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}

	batch, err := New().CaptureObservedInputs(context.Background(), path, 0, lines)
	if err != nil {
		t.Fatalf("capture on real transcript %s: %v", filepath.Base(src), err)
	}
	var reqs, obs int
	for _, ev := range batch.Turns {
		reqs += len(ev.Requests)
		obs += len(ev.Observations)
	}
	t.Logf("real transcript %s: %d lines -> requests=%d observations=%d contents=%d ancestry=%d",
		filepath.Base(src), lines, reqs, obs, len(batch.Contents), len(batch.Ancestry))
	if reqs == 0 {
		t.Fatal("no request evidence extracted from a real transcript")
	}
	if obs == 0 {
		t.Fatal("no observed-input observations extracted from a real transcript with attachments")
	}
}

// End-to-end through the real capture lifecycle: a turn launched in a registered
// repo captures a sample transcript and packages a bundle carrying observed_input.
func TestObservedInput_EndToEndSampleThroughLifecycle(t *testing.T) {
	src := findSampleTranscript(t)
	if src == "" {
		t.Skip("no real sample Claude transcript with attachments on this machine")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	// The Claude project-path decoder round-trips '-' <-> '/', so the repo and home
	// must live under a dash-free base for the encoded transcript path to decode
	// back to the launch repository.
	tmpRoot := "/tmp"
	if evaled, err := filepath.EvalSymlinks(tmpRoot); err == nil {
		tmpRoot = evaled
	}
	base := filepath.Join(tmpRoot, "semantica_oi_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	if strings.Contains(base, "-") {
		t.Skipf("temp base contains '-', cannot encode Claude project path: %s", base)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	fakeHome := filepath.Join(base, "home")
	semHome := filepath.Join(base, "sem")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(semHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// os.UserHomeDir drives the transcript project-path decoder; isolate it.
	t.Setenv("HOME", fakeHome)
	t.Setenv("SEMANTICA_HOME", semHome)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	repoPath := filepath.Join(base, "repo")
	newRegisteredRepo(t, repoPath)
	bh, err := broker.Open(ctx, filepath.Join(semHome, "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()
	if err := broker.Register(ctx, bh, repoPath, repoPath); err != nil {
		t.Fatal(err)
	}
	blobStore, err := blobs.NewStore(filepath.Join(semHome, "objects"))
	if err != nil {
		t.Fatal(err)
	}

	// Place the transcript exactly where Claude would: under
	// ~/.claude/projects/<encoded-repo-path>/<session>.jsonl, so routing recovers
	// the launch repository the way it does for a real session.
	sessionID := uuid.NewString()
	projectsDir := filepath.Join(fakeHome, ".claude", "projects", strings.ReplaceAll(repoPath, "/", "-"))
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptRef := filepath.Join(projectsDir, sessionID+".jsonl")
	provider := New()

	// Prompt submitted before the transcript exists: capture baselines at offset 0.
	prompt := &hooks.Event{
		Type: hooks.PromptSubmitted, SessionID: sessionID, TranscriptRef: transcriptRef,
		Prompt: "end-to-end sample", CWD: repoPath, ProviderTurnID: "gen", Timestamp: time.Now().UnixMilli(),
	}
	if err := hooks.Dispatch(ctx, provider, prompt, bh, blobStore); err != nil {
		t.Fatalf("prompt dispatch: %v", err)
	}

	// The real transcript now lands, exactly as a live session would write it.
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptRef, data, 0o644); err != nil {
		t.Fatal(err)
	}

	stop := &hooks.Event{
		Type: hooks.AgentCompleted, SessionID: sessionID, TranscriptRef: transcriptRef,
		CWD: repoPath, ProviderTurnID: "gen", Timestamp: time.Now().UnixMilli(),
	}
	if err := hooks.Dispatch(ctx, provider, stop, bh, blobStore); err != nil {
		t.Fatalf("stop dispatch: %v", err)
	}

	// Inspect the packaged bundles for a resolved observed_input section.
	h, err := sqlstore.Open(ctx, filepath.Join(repoPath, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	rows, err := h.DB.QueryContext(ctx, `select provenance_bundle_hash from provenance_manifests where kind='turn_bundle' and provenance_bundle_hash is not null`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	bs, err := blobs.NewStore(filepath.Join(repoPath, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	var bundles int
	var withObserved int
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatal(err)
		}
		bundles++
		raw, err := bs.Get(ctx, hash)
		if err != nil {
			t.Fatalf("bundle blob %s missing: %v", hash, err)
		}
		if bytes.Contains(raw, []byte(`"observed_input"`)) {
			withObserved++
			t.Logf("bundle %s carries observed_input: %s", hash[:12], observedInputSummary(raw))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("packaged %d turn bundle(s), %d with observed_input", bundles, withObserved)
	if bundles == 0 {
		t.Fatal("no turn bundle packaged from the sample transcript")
	}
	if withObserved == 0 {
		t.Fatal("no packaged bundle carried observed_input evidence")
	}
}

// observedInputSummary extracts the observed_input object substring for logging.
func observedInputSummary(bundle []byte) string {
	i := bytes.Index(bundle, []byte(`"observed_input"`))
	if i < 0 {
		return ""
	}
	end := i + 200
	if end > len(bundle) {
		end = len(bundle)
	}
	return strings.ReplaceAll(string(bundle[i:end]), "\n", " ")
}

// newRegisteredRepo initializes a git repo at repoPath with an enabled, migrated
// lineage DB and a repository row.
func newRegisteredRepo(t *testing.T, repoPath string) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	semDir := filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: uuid.NewString(), RootPath: repoPath, CreatedAt: 1000, EnabledAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}
}
