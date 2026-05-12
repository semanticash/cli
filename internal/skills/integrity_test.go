package skills

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// sampleSkill is the canonical author-time SKILL.md shape: real
// frontmatter keys, both placeholders unset, a body that contains
// content the hash is meant to cover. It mirrors the on-disk shape
// of skills/skills/semantica-handoff/SKILL.md.
const sampleSkill = `---
name: semantica-handoff
description: |
  Use when the user wants to start a fresh agent session preserving
  the current task context.
x-semantica-managed: true
x-semantica-cli-version: SEMANTICA_CLI_VERSION_PLACEHOLDER
x-semantica-content-hash: sha256:PLACEHOLDER
---

# Semantica Handoff

Body content that the hash must cover.
`

func TestStamp_SubstitutesVersionAndHash(t *testing.T) {
	out, hash, err := Stamp([]byte(sampleSkill), "v0.3.9")
	if err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "x-semantica-cli-version: v0.3.9") {
		t.Errorf("CLI version not substituted into output:\n%s", s)
	}
	if strings.Contains(s, CLIVersionPlaceholder) {
		t.Errorf("CLI version placeholder leaked into final output:\n%s", s)
	}
	if strings.Contains(s, "sha256:PLACEHOLDER") {
		t.Errorf("hash placeholder leaked into final output:\n%s", s)
	}
	if !strings.HasPrefix(hash, hashPrefix) || len(hash) != len(hashPrefix)+64 {
		t.Errorf("hash %q does not look like sha256:<hex64>", hash)
	}
	if !strings.Contains(s, hash) {
		t.Errorf("returned hash %q not present in output:\n%s", hash, s)
	}
}

func TestStamp_RoundTripsThroughVerify(t *testing.T) {
	out, _, err := Stamp([]byte(sampleSkill), "v0.3.9")
	if err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	ok, err := Verify(out)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Errorf("freshly stamped file failed Verify")
	}
}

func TestVerify_DetectsBodyEdit(t *testing.T) {
	out, _, _ := Stamp([]byte(sampleSkill), "v0.3.9")
	edited := bytes.Replace(out, []byte("Body content that the hash must cover."),
		[]byte("Body content that the hash must cover. Edited."), 1)
	if bytes.Equal(out, edited) {
		t.Fatal("test setup: edit did not change file")
	}
	ok, err := Verify(edited)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Errorf("body edit was not detected by Verify")
	}
}

func TestVerify_DetectsFrontmatterEdit(t *testing.T) {
	out, _, _ := Stamp([]byte(sampleSkill), "v0.3.9")
	edited := bytes.Replace(out, []byte("name: semantica-handoff"),
		[]byte("name: semantica-handoff-evil"), 1)
	if bytes.Equal(out, edited) {
		t.Fatal("test setup: edit did not change file")
	}
	ok, err := Verify(edited)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Errorf("frontmatter edit was not detected by Verify")
	}
}

func TestVerify_DetectsDescriptionEdit(t *testing.T) {
	// Editing within the multi-line description block (a YAML block
	// scalar) must still flip the hash. Continuation lines are part
	// of the hashed content even though splitYAMLLine skips them.
	out, _, _ := Stamp([]byte(sampleSkill), "v0.3.9")
	edited := bytes.Replace(out,
		[]byte("preserving"),
		[]byte("preserving and tampering"), 1)
	ok, err := Verify(edited)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Errorf("description-block edit was not detected")
	}
}

func TestVerify_RefusesUnmanagedFile(t *testing.T) {
	s := strings.Replace(sampleSkill, "x-semantica-managed: true\n", "", 1)
	_, err := Verify([]byte(s))
	if !errors.Is(err, ErrManagedMarkerMissing) {
		t.Errorf("expected ErrManagedMarkerMissing, got %v", err)
	}
}

func TestVerify_RefusesMarkedFileWithoutHashLine(t *testing.T) {
	s := strings.Replace(sampleSkill,
		"x-semantica-content-hash: sha256:PLACEHOLDER\n", "", 1)
	_, err := Verify([]byte(s))
	if !errors.Is(err, ErrContentHashMissing) {
		t.Errorf("expected ErrContentHashMissing, got %v", err)
	}
}

func TestStamp_RefusesUnmanagedFile(t *testing.T) {
	s := strings.Replace(sampleSkill, "x-semantica-managed: true\n", "", 1)
	_, _, err := Stamp([]byte(s), "v0.3.9")
	if !errors.Is(err, ErrManagedMarkerMissing) {
		t.Errorf("expected ErrManagedMarkerMissing, got %v", err)
	}
}

func TestStamp_RefusesEmptyCLIVersion(t *testing.T) {
	_, _, err := Stamp([]byte(sampleSkill), "")
	if !errors.Is(err, ErrCLIVersionEmpty) {
		t.Errorf("expected ErrCLIVersionEmpty, got %v", err)
	}
}

func TestStamp_DeterministicAcrossRuns(t *testing.T) {
	out1, hash1, err := Stamp([]byte(sampleSkill), "v0.3.9")
	if err != nil {
		t.Fatalf("Stamp 1: %v", err)
	}
	out2, hash2, err := Stamp([]byte(sampleSkill), "v0.3.9")
	if err != nil {
		t.Fatalf("Stamp 2: %v", err)
	}
	if hash1 != hash2 {
		t.Errorf("hash differed across runs: %q vs %q", hash1, hash2)
	}
	if !bytes.Equal(out1, out2) {
		t.Errorf("Stamp output not byte-identical across runs")
	}
}

func TestStamp_HashChangesWithCLIVersion(t *testing.T) {
	// The CLI version is part of the hashed content, so two installs
	// that differ only in the CLI version must produce different
	// hashes. Without this, a stale install written by an older CLI
	// could pass verification under a newer CLI.
	_, hash1, _ := Stamp([]byte(sampleSkill), "v0.3.9")
	_, hash2, _ := Stamp([]byte(sampleSkill), "v0.4.0")
	if hash1 == hash2 {
		t.Errorf("hash did not change with CLI version: %q", hash1)
	}
}

func TestVerify_AcceptsCRLFInput(t *testing.T) {
	// SKILL.md authored on Windows (or checked out with autocrlf)
	// can have CRLF endings. Verify must canonicalize before
	// hashing so cross-platform installs don't fail.
	out, _, _ := Stamp([]byte(sampleSkill), "v0.3.9")
	withCRLF := bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	ok, err := Verify(withCRLF)
	if err != nil {
		t.Fatalf("Verify on CRLF input: %v", err)
	}
	if !ok {
		t.Errorf("CRLF round trip failed verification")
	}
}

func TestStamp_PreservesTrailingNewline(t *testing.T) {
	// File without trailing newline should stay that way after Stamp
	// so we don't introduce gratuitous diffs into the install path.
	noTrail := strings.TrimRight(sampleSkill, "\n")
	out, _, err := Stamp([]byte(noTrail), "v0.3.9")
	if err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	if bytes.HasSuffix(out, []byte("\n")) {
		t.Errorf("Stamp added a trailing newline; output ends with %q", out[len(out)-3:])
	}
}

func TestStamp_RefusesMissingVersionLine(t *testing.T) {
	s := strings.Replace(sampleSkill,
		"x-semantica-cli-version: SEMANTICA_CLI_VERSION_PLACEHOLDER\n",
		"", 1)
	_, _, err := Stamp([]byte(s), "v0.3.9")
	if !errors.Is(err, ErrCLIVersionMissing) {
		t.Errorf("expected ErrCLIVersionMissing, got %v", err)
	}
}

func TestStamp_RefusesNonPlaceholderVersion(t *testing.T) {
	// Hand-authored or pre-stamped file with a concrete version.
	// Stamp must refuse so the install pipeline cannot accidentally
	// "re-stamp" content that wasn't authored against the
	// placeholder contract.
	s := strings.Replace(sampleSkill,
		"x-semantica-cli-version: SEMANTICA_CLI_VERSION_PLACEHOLDER",
		"x-semantica-cli-version: v0.1.0", 1)
	_, _, err := Stamp([]byte(s), "v0.3.9")
	if !errors.Is(err, ErrCLIVersionNotPlaceholder) {
		t.Errorf("expected ErrCLIVersionNotPlaceholder, got %v", err)
	}
}

func TestStamp_RefusesMissingHashLine(t *testing.T) {
	s := strings.Replace(sampleSkill,
		"x-semantica-content-hash: sha256:PLACEHOLDER\n",
		"", 1)
	_, _, err := Stamp([]byte(s), "v0.3.9")
	if !errors.Is(err, ErrContentHashMissing) {
		t.Errorf("expected ErrContentHashMissing, got %v", err)
	}
}

func TestStamp_RefusesNonPlaceholderHash(t *testing.T) {
	s := strings.Replace(sampleSkill,
		"x-semantica-content-hash: sha256:PLACEHOLDER",
		"x-semantica-content-hash: sha256:0123456789abcdef", 1)
	_, _, err := Stamp([]byte(s), "v0.3.9")
	if !errors.Is(err, ErrContentHashNotPlaceholder) {
		t.Errorf("expected ErrContentHashNotPlaceholder, got %v", err)
	}
}

func TestStamp_LeavesBodyPlaceholderTextUntouched(t *testing.T) {
	// AUTHORING.md and skill bodies might mention the literal
	// placeholder string in prose. Stamp must rewrite only the
	// frontmatter version line, not every occurrence in the file.
	src := `---
name: semantica-handoff
description: example
x-semantica-managed: true
x-semantica-cli-version: SEMANTICA_CLI_VERSION_PLACEHOLDER
x-semantica-content-hash: sha256:PLACEHOLDER
---

# Body

Authors leave x-semantica-cli-version: SEMANTICA_CLI_VERSION_PLACEHOLDER
in source. The publish tooling stamps it at release time.
`
	out, _, err := Stamp([]byte(src), "v0.3.9")
	if err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	s := string(out)

	// Frontmatter line should be substituted exactly once.
	if !strings.Contains(s, "x-semantica-cli-version: v0.3.9") {
		t.Errorf("frontmatter version line not stamped:\n%s", s)
	}
	// Body sentence must still carry the literal placeholder.
	bodyMarker := "Authors leave x-semantica-cli-version: SEMANTICA_CLI_VERSION_PLACEHOLDER"
	if !strings.Contains(s, bodyMarker) {
		t.Errorf("body placeholder text was rewritten by Stamp:\n%s", s)
	}
	// Sanity: total occurrences of the literal placeholder string
	// in the output should equal the number in the body (one), not
	// zero (over-rewritten) or two (frontmatter not rewritten).
	if got := strings.Count(s, "SEMANTICA_CLI_VERSION_PLACEHOLDER"); got != 1 {
		t.Errorf("placeholder count = %d, want 1 (body kept, frontmatter stamped)", got)
	}
}

func TestVerify_RejectsManagedMarkerOtherTruthyForms(t *testing.T) {
	// "x-semantica-managed: yes" is YAML-truthy but our marker
	// requires the literal string "true" so authoring stays
	// unambiguous and the install/uninstall logic does not need a
	// YAML parser to interpret it.
	s := strings.Replace(sampleSkill,
		"x-semantica-managed: true",
		"x-semantica-managed: yes", 1)
	_, err := Verify([]byte(s))
	if !errors.Is(err, ErrManagedMarkerMissing) {
		t.Errorf("expected ErrManagedMarkerMissing for yes, got %v", err)
	}
}
