package provenance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/observedinput"
)

const observedInputFixtureDir = "testdata/observed_input_upload_v1"

// Both CLI and API pin this digest to detect fixture and manifest changes.
const observedInputManifestSHA256 = "e05625bb060ade683703165469e4aef7ee56fe2bd8e5981a4f82a0d2042640c0"

// Verify fixture bytes against the manifest and its pinned digest.
func TestObservedInputUploadFixture_MatchesManifest(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join(observedInputFixtureDir, "manifest.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256Hex(manifest); got != observedInputManifestSHA256 {
		t.Fatalf("manifest digest %s, pinned %s (update the pin only for a reviewed contract change)", got, observedInputManifestSHA256)
	}
	lines := 0
	sc := bufio.NewScanner(bytes.NewReader(manifest))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("bad manifest line: %q", line)
		}
		wantHash, rel := fields[0], fields[1]
		b, err := os.ReadFile(filepath.Join(observedInputFixtureDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("manifest file %s: %v", rel, err)
		}
		if got := sha256Hex(b); got != wantHash {
			t.Fatalf("%s: hash %s, manifest says %s", rel, got, wantHash)
		}
		lines++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if lines == 0 {
		t.Fatal("empty manifest")
	}
}

// Content objects are content-addressed: each file name is its own SHA-256.
func TestObservedInputUploadFixture_ContentIsAddressed(t *testing.T) {
	for _, side := range []string{"local", "outbound"} {
		dir := filepath.Join(observedInputFixtureDir, side, "content")
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if got := sha256Hex(b); got != e.Name() {
				t.Fatalf("%s/%s: content hash %s does not match name", side, e.Name(), got)
			}
		}
	}
}

// Outbound references resolve locally; withheld content has a marker and no references.
func TestObservedInputUploadFixture_OutboundClosureAndWithholding(t *testing.T) {
	var ev observedinput.Evidence
	readFixtureJSON(t, filepath.Join("outbound", "observed_input.json"), &ev)

	present := func(hash string) bool {
		if hash == "" {
			return false
		}
		_, err := os.Stat(filepath.Join(observedInputFixtureDir, "outbound", "content", hash))
		return err == nil
	}
	if ev.UploadTransformVersion == 0 {
		t.Fatal("outbound document missing upload_transform_version")
	}
	for _, r := range ev.Requests {
		if !present(r.InstructionRef) {
			t.Fatalf("request %s instruction_ref not in outbound objects: %q", r.ID, r.InstructionRef)
		}
	}
	// Accept observation gaps or document gaps naming this delivery.
	hasGap := func(o observedinput.ObservedInput) bool {
		if len(o.Gaps) > 0 {
			return true
		}
		for _, g := range ev.Gaps {
			if g.Subject == o.DeliveryID {
				return true
			}
		}
		return false
	}

	var sawWithheld, sawAbsent, sawPresent bool
	for _, o := range ev.Observations {
		rep := o.Representation
		switch {
		case rep.Upload != nil:
			// Withholding preserves capture state and size.
			sawWithheld = true
			if rep.Upload.State != "withheld" || rep.Upload.Reason == "" {
				t.Fatalf("%s: malformed upload marker %+v", o.DeliveryID, rep.Upload)
			}
			if rep.State != observedinput.RepPresent || rep.ContentSize == 0 {
				t.Fatalf("%s: withholding must preserve captured state and size", o.DeliveryID)
			}
			if rep.ContentRef != "" || rep.SourceContentRef != "" {
				t.Fatalf("%s: withheld representation must not carry content references", o.DeliveryID)
			}
		case rep.State == observedinput.RepUnavailable || rep.State == observedinput.RepReferenceOnly:
			// Absent content requires a gap and no references.
			sawAbsent = true
			if rep.ContentRef != "" || rep.SourceContentRef != "" {
				t.Fatalf("%s: absent (%s) content must not carry a content reference", o.DeliveryID, rep.State)
			}
			if rep.Upload != nil {
				t.Fatalf("%s: absent content is a capture gap, not withholding", o.DeliveryID)
			}
			if !hasGap(o) {
				t.Fatalf("%s: absent content must carry an explicit gap", o.DeliveryID)
			}
		case rep.State == observedinput.RepPresent:
			// Present content requires resolvable references.
			sawPresent = true
			if !present(rep.ContentRef) {
				t.Fatalf("%s: content_ref not in outbound objects: %q", o.DeliveryID, rep.ContentRef)
			}
			if rep.SourceContentRef != "" && !present(rep.SourceContentRef) {
				t.Fatalf("%s: source_content_ref not in outbound objects: %q", o.DeliveryID, rep.SourceContentRef)
			}
		default:
			t.Fatalf("%s: unexpected representation state %q", o.DeliveryID, rep.State)
		}
	}
	if !sawPresent || !sawWithheld || !sawAbsent {
		t.Fatalf("fixture must exercise present, withheld, and absent content (present=%v withheld=%v absent=%v)", sawPresent, sawWithheld, sawAbsent)
	}
}

// No test secret or private path survives into any outbound file.
func TestObservedInputUploadFixture_NoSecretLeak(t *testing.T) {
	forbidden := []string{
		"ghp_0123456789abcdef0123456789abcdef0123", // planted secret
		"s3cr3t",     // URL credential
		"/Users/dev", // private transcript directory
		"token=abc",  // URL query secret
	}
	root := filepath.Join(observedInputFixtureDir, "outbound")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, s := range forbidden {
			if bytes.Contains(b, []byte(s)) {
				t.Fatalf("%s leaks %q into outbound", path, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readFixtureJSON(t *testing.T, rel string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(observedInputFixtureDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}
