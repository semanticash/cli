// Package testutil provides the shared golden-file harness for the
// direct-emit hook providers under internal/hooks/<provider>. The
// goal is a permanent regression net for the shared builder package
// in internal/hooks/builder: any change that alters the emitted
// broker.RawEvent slice or the stored blob contents for any provider
// fails the golden comparison on every provider that exercises the
// affected code path.
//
// Goldens are committed to each provider's testdata/direct_emit
// directory as one JSON file per case. The file holds the expected
// []broker.RawEvent and a content-addressed map of blob payloads
// that were stored during the run. A mismatch in either field is a
// test failure.
//
// Regenerating goldens rewrites committed fixtures. Run
// `go test ./internal/hooks/... -update`, review the diff before
// committing, and describe the reason for the fixture change in the
// commit message.
//
// The harness also guards against silent coverage loss by checking
// that the set of .golden.json files on disk matches the set of
// cases declared in code. Stale fixtures fail the run in both
// comparison and update modes; a regeneration run with an orphaned
// file will still write the valid fixtures but will mark the run as
// failed so the orphan shows up in the output. Delete the stale
// file and rerun to get a clean pass.
package testutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
)

const goldenSuffix = ".golden.json"

// update is set by the -update flag passed to `go test`. When true,
// RunGolden writes the emitted output back to the golden file
// instead of comparing against it.
var update = flag.Bool("update", false, "update golden files with the emitter's current output")

// Case describes one input-to-output mapping that the golden harness
// exercises. Name is both the human-readable label in test output
// and the basename of the golden file (Name + ".golden.json").
type Case struct {
	// Name is the fixture label. Becomes the golden file name.
	Name string

	// Description is an optional free-form string recorded in the
	// golden file for readers scanning the fixtures. It does not
	// participate in comparison.
	Description string

	// Event is the hook event the provider processes. Constructed in
	// the test file rather than loaded from disk so the types are
	// checked at compile time and the fixtures read like code.
	Event *hooks.Event
}

// CASBlobPutter is a content-addressed fake BlobPutter. Each Put
// returns the hex SHA-256 of the payload, which means the same
// content always produces the same hash regardless of Put order or
// the number of stored blobs. That stability is what makes the
// golden files meaningful across refactors that might legitimately
// reorder blob writes inside a builder.
type CASBlobPutter struct {
	Stored map[string][]byte
}

// NewCASBlobPutter returns a CASBlobPutter with an initialized store.
func NewCASBlobPutter() *CASBlobPutter {
	return &CASBlobPutter{Stored: make(map[string][]byte)}
}

// Put records the payload under its content hash and returns the
// hash. Duplicate writes collapse to one entry.
func (c *CASBlobPutter) Put(_ context.Context, b []byte) (string, int64, error) {
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	if _, ok := c.Stored[h]; !ok {
		c.Stored[h] = append([]byte(nil), b...)
	}
	return h, int64(len(b)), nil
}

// goldenDoc is the serialized shape of one golden file. The outer
// envelope carries the fixture name and description so a reviewer
// looking at the file alone can see what case it represents;
// comparison only cares about Events and Blobs.
type goldenDoc struct {
	Name           string              `json:"name"`
	Description    string              `json:"description,omitempty"`
	ExpectedEvents []broker.RawEvent   `json:"expected_events"`
	ExpectedBlobs  map[string]string   `json:"expected_blobs"`
}

// RunGolden iterates the cases against the provider, compares each
// result against the matching golden file, and either fails the test
// or rewrites the file depending on the -update flag.
//
// dir is the directory relative to the provider test file that holds
// the golden files (typically "testdata/direct_emit"). The directory
// is created when -update is set, so a fresh provider can generate
// its initial goldens without manual setup.
//
// Before running any case, RunGolden validates that the on-disk
// fixtures match the declared case names exactly. A stale .golden.json
// file from a removed or renamed case would otherwise silently reduce
// coverage: the removed case no longer runs, the stale file lingers,
// and the test suite keeps passing with fewer assertions than the
// maintainer thinks. This check makes the drift loud.
func RunGolden(t *testing.T, emitter hooks.DirectHookEmitter, dir string, cases []Case) {
	t.Helper()

	if *update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create golden dir %s: %v", dir, err)
		}
	}

	missing, extra, err := diffFixtureSet(dir, cases)
	if err != nil {
		t.Fatalf("inspect fixture directory %s: %v", dir, err)
	}
	if len(extra) > 0 {
		msg := fmt.Sprintf("stale golden files in %s (delete before regenerating): %v", dir, extra)
		if *update {
			// Use Errorf rather than Logf: t.Logf only surfaces under
			// `go test -v`, and the documented regeneration command
			// does not set -v. A silent warning would defeat the
			// point of this check. Errorf (rather than Fatalf) marks
			// the run as failed without aborting, so the remaining
			// valid cases still get regenerated in the same pass and
			// the maintainer sees the failure signal plus the fresh
			// fixtures at once.
			t.Errorf("%s", msg)
		} else {
			t.Fatalf("%s", msg)
		}
	}
	if len(missing) > 0 && !*update {
		t.Fatalf("declared cases have no golden file in %s (run `go test -update` to create): %v",
			dir, missing)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Helper()

			bs := NewCASBlobPutter()
			events, err := emitter.BuildHookEvents(context.Background(), tc.Event, bs)
			if err != nil {
				t.Fatalf("BuildHookEvents returned error: %v", err)
			}

			// Normalize nil slices to empty slices so they round-trip
			// through JSON without flipping between null and [] in
			// the golden file.
			if events == nil {
				events = []broker.RawEvent{}
			}

			blobs := stringifyBlobs(bs.Stored)

			goldenPath := filepath.Join(dir, tc.Name+".golden.json")

			if *update {
				writeGolden(t, goldenPath, tc, events, blobs)
				return
			}

			wantDoc, err := readGolden(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run `go test -update` to create it)", goldenPath, err)
			}

			if !reflect.DeepEqual(events, wantDoc.ExpectedEvents) {
				gotJSON := marshalPretty(events)
				wantJSON := marshalPretty(wantDoc.ExpectedEvents)
				t.Errorf("events mismatch for %s\n--- got ---\n%s\n--- want ---\n%s",
					tc.Name, gotJSON, wantJSON)
			}

			if !reflect.DeepEqual(blobs, wantDoc.ExpectedBlobs) {
				t.Errorf("blobs mismatch for %s\n--- got ---\n%s\n--- want ---\n%s",
					tc.Name, marshalPretty(blobs), marshalPretty(wantDoc.ExpectedBlobs))
			}
		})
	}
}

// stringifyBlobs converts the byte-valued CAS store into a string-
// valued map so the golden file is human-readable. The byte slices
// produced by the providers are always UTF-8 JSON or UTF-8 text, so
// no base64 indirection is needed.
func stringifyBlobs(stored map[string][]byte) map[string]string {
	if len(stored) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(stored))
	for k, v := range stored {
		out[k] = string(v)
	}
	return out
}

func readGolden(path string) (*goldenDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc goldenDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse golden %s: %w", path, err)
	}
	if doc.ExpectedEvents == nil {
		doc.ExpectedEvents = []broker.RawEvent{}
	}
	if doc.ExpectedBlobs == nil {
		doc.ExpectedBlobs = map[string]string{}
	}
	return &doc, nil
}

func writeGolden(t *testing.T, path string, tc Case, events []broker.RawEvent, blobs map[string]string) {
	t.Helper()
	doc := goldenDoc{
		Name:           tc.Name,
		Description:    tc.Description,
		ExpectedEvents: events,
		ExpectedBlobs:  blobs,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden %s: %v", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write golden %s: %v", path, err)
	}
	t.Logf("wrote golden %s", path)
}

func marshalPretty(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(data)
}


// diffFixtureSet returns the names of golden files that are declared
// by cases but absent from dir (missing) and the names of golden
// files present in dir but not declared by any case (extra). Files
// that do not end in .golden.json are ignored so unrelated artifacts
// in the same testdata directory do not trigger false positives.
//
// A non-existent directory is not an error when no cases are
// declared, and produces a fully-missing result when some are. The
// caller distinguishes the update-mode-regeneration case (where
// missing files will be created) from the comparison case (where
// missing files are a hard failure).
func diffFixtureSet(dir string, cases []Case) (missing, extra []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No directory yet. Every declared case is missing;
			// nothing can be extra.
			for _, tc := range cases {
				missing = append(missing, tc.Name+goldenSuffix)
			}
			sort.Strings(missing)
			return missing, nil, nil
		}
		return nil, nil, err
	}

	onDisk := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, goldenSuffix) {
			continue
		}
		onDisk[name] = true
	}

	declared := make(map[string]bool, len(cases))
	for _, tc := range cases {
		declared[tc.Name+goldenSuffix] = true
	}

	for name := range declared {
		if !onDisk[name] {
			missing = append(missing, name)
		}
	}
	for name := range onDisk {
		if !declared[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra, nil
}
