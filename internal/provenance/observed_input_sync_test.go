package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/observedinput"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// Sync includes sanitized input evidence and withholds binary and ignored-file content.
func TestSyncPendingTurns_ObservedInputDelivery(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	r := newObservedRepo(t)
	gitInitRepo(t, r.path)
	if err := os.WriteFile(filepath.Join(r.path, ".gitignore"), []byte("private.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bs, err := blobs.NewStore(filepath.Join(r.path, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(b []byte) string {
		h, _, err := bs.Put(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	instr := []byte("review the config change\n")
	normal := []byte("normal file body visible in upload\n")
	ignoredBody := []byte("PRIVATE_MARKER must never leave the machine\n")
	pdf := []byte("%PDF-1.4\nbinary\n%%EOF\n")
	hInstr, hNormal, hIgnored, hPDF := put(instr), put(normal), put(ignoredBody), put(pdf)

	turnID := "turn-sync-0001"
	ev := observedinput.Evidence{
		Version: 1, Provider: "claude_code", SessionID: r.sessionID, TurnID: turnID,
		Requests: []observedinput.RequestEvent{{
			ID: "R1", ProviderEventID: "req-uuid", Provider: "claude_code", SessionID: r.sessionID, TurnID: turnID,
			Origin: observedinput.OriginHuman, InstructionRef: hInstr, Ordinal: 1,
			Source: observedinput.SourceRef{Locator: "/home/dev/.claude/x.jsonl", Position: 1, Native: "req-uuid"},
		}},
		Observations: []observedinput.ObservedInput{
			{DeliveryID: "O1", Provider: "claude_code", SessionID: r.sessionID, TurnID: turnID,
				Acquisition: observedinput.AcquisitionAttachment, Scope: observedinput.ScopeRequestEnvelope, Ordinal: 2,
				Representation: observedinput.Representation{State: observedinput.RepPresent, ContentRef: hNormal, ContentSize: int64(len(normal)), MediaType: "text/plain"},
				InputSource:    observedinput.InputSource{Kind: "file", Locator: filepath.Join(r.path, "normal.txt")}},
			{DeliveryID: "O2", Provider: "claude_code", SessionID: r.sessionID, TurnID: turnID,
				Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext, ToolCallID: "toolu_1", Ordinal: 3,
				Representation: observedinput.Representation{State: observedinput.RepPresent, ContentRef: hIgnored, ContentSize: int64(len(ignoredBody)), MediaType: "text/plain"},
				InputSource:    observedinput.InputSource{Kind: "file", Locator: filepath.Join(r.path, "private.txt")}},
			{DeliveryID: "O3", Provider: "claude_code", SessionID: r.sessionID, TurnID: turnID,
				Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext, ToolCallID: "toolu_2", Ordinal: 4,
				Representation: observedinput.Representation{State: observedinput.RepPresent, ContentRef: hPDF, ContentSize: int64(len(pdf)), MediaType: "application/pdf"},
				InputSource:    observedinput.InputSource{Kind: "file", Locator: filepath.Join(r.path, "doc.pdf")}},
		},
	}
	evDoc, _ := json.Marshal(ev)
	evHash := put(evDoc)

	bundle := map[string]any{
		"version": 1, "provider": "claude_code", "session_id": r.sessionID, "turn_id": turnID,
		"observed_input": map[string]any{"version": 1, "evidence_hash": evHash, "requests": 1, "observations": 3},
	}
	bundleDoc, _ := json.Marshal(bundle)
	bundleHash := put(bundleDoc)

	seedPackagedManifest(t, r, turnID, bundleHash)
	if err := util.WriteSettings(filepath.Join(r.path, ".semantica"), util.Settings{ConnectedRepoID: "conn-1"}); err != nil {
		t.Fatal(err)
	}

	results, err := SyncPendingTurns(ctx, r.path, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var res *SyncResult
	for i := range results {
		if results[i].TurnID == turnID {
			res = &results[i]
		}
	}
	if res == nil || res.Skipped {
		t.Fatalf("turn not synced: %+v", results)
	}

	var env struct {
		Objects []struct {
			Kind string `json:"kind"`
			Hash string `json:"hash"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(res.Envelope, &env); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	var docHash, uploadedBundleHash string
	for _, o := range env.Objects {
		kinds[o.Kind]++
		switch o.Kind {
		case "observed_input":
			docHash = o.Hash
		case "bundle":
			uploadedBundleHash = o.Hash
		}
	}
	// The bundle must reference the uploaded document.
	if uploadedBundleHash == "" {
		t.Fatal("no bundle object in envelope")
	}
	var uploadedBundle struct {
		ObservedInput struct {
			EvidenceHash string `json:"evidence_hash"`
		} `json:"observed_input"`
	}
	if err := json.Unmarshal(res.RedactedBlobs[uploadedBundleHash], &uploadedBundle); err != nil {
		t.Fatal(err)
	}
	if uploadedBundle.ObservedInput.EvidenceHash != docHash {
		t.Fatalf("bundle evidence_hash %q does not point at the emitted document %q", uploadedBundle.ObservedInput.EvidenceHash, docHash)
	}
	if uploadedBundle.ObservedInput.EvidenceHash == evHash {
		t.Fatal("bundle still points at the untransformed local document")
	}
	if kinds["observed_input"] != 1 {
		t.Fatalf("expected 1 observed_input object, got %d", kinds["observed_input"])
	}
	if kinds["observed_input_content"] == 0 {
		t.Fatal("expected observed_input_content objects")
	}

	// Only the allowed text content may appear in upload objects.
	for _, blob := range res.RedactedBlobs {
		if bytes.Contains(blob, ignoredBody) {
			t.Fatal("ignored-file content leaked into an uploaded object")
		}
		if bytes.Contains(blob, pdf) {
			t.Fatal("withheld PDF bytes leaked into an uploaded object")
		}
	}
	if _, ok := res.RedactedBlobs[hIgnored]; ok {
		t.Fatal("ignored content object present among uploads")
	}
	if _, ok := res.RedactedBlobs[hPDF]; ok {
		t.Fatal("withheld PDF object present among uploads")
	}
	if _, ok := res.RedactedBlobs[hNormal]; !ok {
		t.Fatal("normal file content missing from uploads")
	}

	// The transformed document withholds O2 (ignored) and O3 (binary) and keeps O1.
	var outDoc *observedinput.Evidence
	for _, o := range env.Objects {
		if o.Kind == "observed_input" {
			var got observedinput.Evidence
			if err := json.Unmarshal(res.RedactedBlobs[o.Hash], &got); err != nil {
				t.Fatal(err)
			}
			outDoc = &got
		}
	}
	if outDoc == nil {
		t.Fatal("no observed_input document among uploads")
	}
	withheld := map[string]bool{}
	for _, o := range outDoc.Observations {
		if o.Representation.Upload != nil {
			withheld[o.DeliveryID] = o.Representation.Upload.State == "withheld"
			if o.Representation.ContentRef != "" {
				t.Fatalf("%s withheld but retained a content ref", o.DeliveryID)
			}
		}
	}
	if !withheld["O2"] || !withheld["O3"] {
		t.Fatalf("expected O2 (ignored) and O3 (binary) withheld, got %+v", withheld)
	}
}

func gitInitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func seedPackagedManifest(t *testing.T, r observedRepo, turnID, bundleHash string) {
	t.Helper()
	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(r.path, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if err := h.Queries.UpsertProvenanceManifest(ctx, sqldb.UpsertProvenanceManifestParams{
		ManifestID:           "manifest-" + turnID,
		RepositoryID:         r.repoID,
		SessionID:            r.sessionID,
		TurnID:               turnID,
		Provider:             "claude_code",
		Kind:                 "turn_bundle",
		ProvenanceBundleHash: sqlstore.NullStr(bundleHash),
		StartedAt:            1,
		Status:               "packaged",
		CreatedAt:            1,
		UpdatedAt:            1,
	}); err != nil {
		t.Fatal(err)
	}
}
