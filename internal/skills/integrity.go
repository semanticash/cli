// Package skills implements the install-time integrity engine for
// Semantica SKILL.md files. The engine is the source of truth for
// the ownership model documented in the skills repo's
// `docs/AUTHORING.md`: every Semantica-managed file carries an
// `x-semantica-managed` marker and a whole-file content hash with
// the hash line itself replaced by a fixed placeholder before
// hashing, so install/uninstall can detect user edits before
// taking destructive action.
//
// The engine is intentionally pure: it operates on byte slices and
// does no filesystem I/O. The install/uninstall commands wrap it.
package skills

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Frontmatter keys the engine reads or writes.
const (
	ManagedKey     = "x-semantica-managed"
	CLIVersionKey  = "x-semantica-cli-version"
	ContentHashKey = "x-semantica-content-hash"
)

// Author-time placeholder values. SKILL.md files in source carry
// these literal strings; the publish/install pipeline substitutes
// real values.
const (
	CLIVersionPlaceholder  = "SEMANTICA_CLI_VERSION_PLACEHOLDER"
	ContentHashPlaceholder = "sha256:PLACEHOLDER"
)

// hashPrefix is the algorithm tag used in the stored hash value.
// Stored hashes always look like "sha256:<hex>".
const hashPrefix = "sha256:"

// ErrManagedMarkerMissing is returned when the file is missing the
// `x-semantica-managed: true` marker. The install/uninstall
// commands surface this as "this file is not Semantica-managed."
var ErrManagedMarkerMissing = errors.New("x-semantica-managed marker missing")

// ErrContentHashMissing is returned when no `x-semantica-content-hash`
// line exists in the frontmatter. A managed file without a hash is
// malformed and the engine refuses to operate on it.
var ErrContentHashMissing = errors.New("x-semantica-content-hash line missing")

// ErrContentHashNotPlaceholder is returned by Stamp when the hash
// line is present but its value is not the author-time placeholder.
// The input has been pre-stamped or hand-edited; refuse rather
// than silently re-hashing.
var ErrContentHashNotPlaceholder = errors.New("x-semantica-content-hash is not the author-time placeholder")

// ErrCLIVersionMissing is returned when no
// `x-semantica-cli-version` line exists in the frontmatter. Stamp
// refuses to install an unversioned file because the version is
// part of the integrity-checked content.
var ErrCLIVersionMissing = errors.New("x-semantica-cli-version line missing")

// ErrCLIVersionNotPlaceholder is returned when the version line is
// present but its value is not the author-time placeholder.
// Catches accidental input of an already-stamped file as well as
// hand-authored files that hard-coded a version string.
var ErrCLIVersionNotPlaceholder = errors.New("x-semantica-cli-version is not the author-time placeholder")

// ErrCLIVersionEmpty is returned when Stamp is called with an empty
// CLI version argument. Empty cliVersion would round-trip to a
// blank value in the installed file.
var ErrCLIVersionEmpty = errors.New("cliVersion is empty")

// Stamp prepares a source SKILL.md for installation. It validates
// that the input is an author-time SKILL.md (managed marker
// present, both placeholders present in the frontmatter), then
// substitutes the version placeholder with the supplied CLI
// version and computes the whole-file content hash with the hash
// line replaced by its fixed placeholder. Returns the final
// installable bytes and the computed hash value (e.g.
// "sha256:abcd...") for diagnostic logging.
//
// Substitution is frontmatter-only and line-targeted: the engine
// rewrites just the value of the `x-semantica-cli-version` line,
// so any literal occurrences of the placeholder string in the
// markdown body (for example, in documentation explaining how
// authoring works) are left untouched. Inputs that have already
// been stamped, or that hand-author a concrete version string,
// are rejected so the install pipeline is never the source of an
// incorrectly-stamped file.
func Stamp(src []byte, cliVersion string) ([]byte, string, error) {
	if cliVersion == "" {
		return nil, "", ErrCLIVersionEmpty
	}
	canonical := canonicalize(src)
	if !hasManagedMarker(canonical) {
		return nil, "", ErrManagedMarkerMissing
	}

	if v, ok := readFrontmatterValue(canonical, CLIVersionKey); !ok {
		return nil, "", ErrCLIVersionMissing
	} else if v != CLIVersionPlaceholder {
		return nil, "", ErrCLIVersionNotPlaceholder
	}
	if v, ok := readFrontmatterValue(canonical, ContentHashKey); !ok {
		return nil, "", ErrContentHashMissing
	} else if v != ContentHashPlaceholder {
		return nil, "", ErrContentHashNotPlaceholder
	}

	versionStamped, err := substituteFrontmatterValue(canonical, CLIVersionKey, cliVersion)
	if err != nil {
		return nil, "", err
	}

	// versionStamped still carries `x-semantica-content-hash:
	// sha256:PLACEHOLDER` because we just validated it. Hashing the
	// version-stamped bytes directly is therefore equivalent to
	// hashing the placeholder-substituted form Verify will compute
	// later, which makes round-trip checks exact.
	sum := sha256.Sum256(versionStamped)
	hashValue := hashPrefix + hex.EncodeToString(sum[:])

	final, err := substituteFrontmatterValue(versionStamped, ContentHashKey, hashValue)
	if err != nil {
		return nil, "", err
	}
	return final, hashValue, nil
}

// Verify reports whether the recomputed content hash of an
// installed SKILL.md matches the value stored in its frontmatter.
// A return of (true, nil) means the file is byte-identical to what
// Stamp produced; (false, nil) means the file has been edited and
// callers should refuse to remove or overwrite it without --force.
//
// ErrManagedMarkerMissing fires when the file lacks the management
// marker entirely; ErrContentHashMissing fires when the marker is
// present but the hash line is missing.
func Verify(installed []byte) (bool, error) {
	canonical := canonicalize(installed)
	if !hasManagedMarker(canonical) {
		return false, ErrManagedMarkerMissing
	}
	stored, ok := readFrontmatterValue(canonical, ContentHashKey)
	if !ok {
		return false, ErrContentHashMissing
	}

	placeholdered, err := substituteFrontmatterValue(canonical, ContentHashKey, ContentHashPlaceholder)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(placeholdered)
	recomputed := hashPrefix + hex.EncodeToString(sum[:])
	return recomputed == stored, nil
}

// canonicalize normalizes line endings to LF so the hash is stable
// across platforms. Stamped output is always LF; Verify accepts
// either. Without this, a Git checkout that converted LF to CRLF on
// Windows would fail every integrity check it performed locally.
func canonicalize(b []byte) []byte {
	if !bytes.Contains(b, []byte("\r\n")) {
		return b
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// hasManagedMarker scans the frontmatter for the literal
// `x-semantica-managed: true` line. The value must be exactly
// "true": "yes", "1", or any other YAML-truthy variant is
// rejected to keep the marker unambiguous.
func hasManagedMarker(b []byte) bool {
	var found bool
	walkFrontmatter(b, func(key, value string) bool {
		if key == ManagedKey && value == "true" {
			found = true
			return true
		}
		return false
	})
	return found
}

// readFrontmatterValue returns the trimmed value associated with
// the given top-level frontmatter key and whether it was found.
// Block-scalar continuation lines, comments, and list items are
// skipped by walkFrontmatter, so the result is unambiguous for
// scalar keys.
func readFrontmatterValue(b []byte, key string) (string, bool) {
	var (
		value string
		found bool
	)
	walkFrontmatter(b, func(k, v string) bool {
		if k == key {
			value = v
			found = true
			return true
		}
		return false
	})
	return value, found
}

// substituteFrontmatterValue returns a copy of b in which the
// frontmatter line for the given key has been replaced by
// "<key>: <newValue>". Other lines are preserved byte-for-byte.
// Returns errKeyNotFound when the key is absent from the
// frontmatter so callers can map it onto a domain-specific error
// (Verify maps to ErrContentHashMissing, etc.).
//
// The trailing-newline state of b is preserved so the rewriter
// does not introduce gratuitous diffs in the install path.
func substituteFrontmatterValue(b []byte, key, newValue string) ([]byte, error) {
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	delimSeen := 0
	inFrontmatter := false
	replaced := false
	for sc.Scan() {
		line := sc.Text()
		if line == "---" {
			delimSeen++
			inFrontmatter = delimSeen == 1
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		if inFrontmatter {
			k, _ := splitYAMLLine(line)
			if k == key {
				out.WriteString(key + ": " + newValue)
				out.WriteByte('\n')
				replaced = true
				continue
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !replaced {
		return nil, errKeyNotFound{key: key}
	}

	final := out.Bytes()
	if !bytes.HasSuffix(b, []byte("\n")) && bytes.HasSuffix(final, []byte("\n")) {
		final = final[:len(final)-1]
	}
	return final, nil
}

// errKeyNotFound is the internal error substituteFrontmatterValue
// returns when the requested key isn't in the frontmatter. It is
// intentionally not exported: callers either pre-validate (Stamp)
// or map it to the domain error (Verify wraps it as
// ErrContentHashMissing). Keeping it private prevents leaking the
// generic error to skill-author docs.
type errKeyNotFound struct{ key string }

func (e errKeyNotFound) Error() string { return "frontmatter key " + e.key + " missing" }

// walkFrontmatter calls visit once per top-level key:value line in
// the frontmatter block (between the opening `---` and the next
// `---`). The walk stops when visit returns true. Continuation
// lines (indented values from block scalars like `description: |`)
// and comment lines are skipped.
func walkFrontmatter(b []byte, visit func(key, value string) bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	delimSeen := 0
	inFrontmatter := false
	for sc.Scan() {
		line := sc.Text()
		if line == "---" {
			delimSeen++
			inFrontmatter = delimSeen == 1
			if delimSeen >= 2 {
				return
			}
			continue
		}
		if !inFrontmatter {
			continue
		}
		key, value := splitYAMLLine(line)
		if key == "" {
			continue
		}
		if visit(key, value) {
			return
		}
	}
}

// splitYAMLLine returns the trimmed key and value sides of a
// "key: value" frontmatter line. Lines that begin with whitespace
// (block scalar continuations, list items) or have no colon return
// empty strings so the caller skips them.
func splitYAMLLine(line string) (string, string) {
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return "", ""
	}
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", ""
	}
	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])
	return key, value
}
