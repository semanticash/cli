package provenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/semanticash/cli/internal/observedinput"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/redact"
	"github.com/semanticash/cli/internal/store/blobs"
)

// Upload limits are variables so tests can exercise smaller boundaries.
var (
	observedInputMaxContentBytes   = 8 << 20  // per content object
	observedInputMaxDocumentBytes  = 4 << 20  // per observed-input document
	observedInputMaxContentObjects = 512      // distinct content objects per turn
	observedInputMaxAggregateBytes = 64 << 20 // total content bytes per turn
)

// observedInputUpload is the transformed, upload-ready evidence for one turn.
type observedInputUpload struct {
	DocHash  string            // hash of the transformed document
	DocBytes []byte            // transformed document (kind "observed_input")
	Content  map[string][]byte // upload hash -> bytes (kind "observed_input_content")
}

// buildObservedInputUpload verifies and sanitizes local evidence for upload.
// Missing or corrupt objects, exceeded limits, and redaction failures return errors.
func buildObservedInputUpload(ctx context.Context, bs *blobs.Store, evidenceHash, repoRoot string) (*observedInputUpload, error) {
	raw, err := bs.Get(ctx, evidenceHash)
	if err != nil {
		return nil, fmt.Errorf("observed-input document %s: %w", shortHash(evidenceHash), err)
	}
	if hashHex(raw) != evidenceHash {
		return nil, fmt.Errorf("observed-input document %s: content hash mismatch", shortHash(evidenceHash))
	}
	if len(raw) > observedInputMaxDocumentBytes {
		return nil, fmt.Errorf("observed-input document %s: %d bytes exceeds limit %d", shortHash(evidenceHash), len(raw), observedInputMaxDocumentBytes)
	}
	var ev observedinput.Evidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("observed-input document %s: %w", shortHash(evidenceHash), err)
	}

	// Exclude outside paths from the Git check. Unconfirmed ignore status withholds content.
	var inRepoPaths []string
	for _, o := range ev.Observations {
		if o.InputSource.Kind == "file" && withinRepo(o.InputSource.Locator, repoRoot) {
			inRepoPaths = append(inRepoPaths, o.InputSource.Locator)
		}
	}
	ignored, ignoreDeterminable := gitIgnoreDecision(ctx, repoRoot, inRepoPaths)

	up := &observedInputUpload{Content: map[string][]byte{}}
	var aggregate int64

	// loadVerified checks the content hash and size limit.
	loadVerified := func(localHash string) ([]byte, error) {
		body, err := bs.Get(ctx, localHash)
		if err != nil {
			return nil, fmt.Errorf("content %s: %w", shortHash(localHash), err)
		}
		if hashHex(body) != localHash {
			return nil, fmt.Errorf("content %s: content hash mismatch", shortHash(localHash))
		}
		if len(body) > observedInputMaxContentBytes {
			return nil, fmt.Errorf("content %s: %d bytes exceeds limit %d", shortHash(localHash), len(body), observedInputMaxContentBytes)
		}
		return body, nil
	}

	// verifyContent validates a referenced object before withholding it.
	verifyContent := func(localHash string) error {
		if localHash == "" {
			return nil
		}
		_, err := loadVerified(localHash)
		return err
	}

	// transformContent verifies, redacts, and deduplicates content by its upload hash.
	transformContent := func(localHash string) (string, int64, error) {
		body, err := loadVerified(localHash)
		if err != nil {
			return "", 0, err
		}
		redacted, err := redact.Bytes(body)
		if err != nil {
			return "", 0, fmt.Errorf("content %s: redaction failed: %w", shortHash(localHash), err)
		}
		uploadHash := hashHex(redacted)
		if _, seen := up.Content[uploadHash]; !seen {
			if len(up.Content) >= observedInputMaxContentObjects {
				return "", 0, fmt.Errorf("observed-input content objects exceed limit %d", observedInputMaxContentObjects)
			}
			aggregate += int64(len(redacted))
			if aggregate > int64(observedInputMaxAggregateBytes) {
				return "", 0, fmt.Errorf("observed-input aggregate content exceeds limit %d", observedInputMaxAggregateBytes)
			}
			up.Content[uploadHash] = redacted
		}
		return uploadHash, int64(len(redacted)), nil
	}

	out := ev
	out.UploadTransformVersion = UploadTransformVersion

	// Redact request instructions before upload.
	out.Requests = append([]observedinput.RequestEvent(nil), ev.Requests...)
	for i := range out.Requests {
		out.Requests[i].Source = sanitizeTranscriptSource(out.Requests[i].Source)
		if ref := out.Requests[i].InstructionRef; ref != "" {
			uploadHash, _, err := transformContent(ref)
			if err != nil {
				return nil, err
			}
			out.Requests[i].InstructionRef = uploadHash
		}
	}

	out.Observations = append([]observedinput.ObservedInput(nil), ev.Observations...)
	for i := range out.Observations {
		o := &out.Observations[i]
		// Preserve the original locator for the withholding decision.
		origSource := o.InputSource
		o.Source = sanitizeTranscriptSource(o.Source)
		o.InputSource = sanitizeInputSource(o.InputSource, repoRoot)
		if len(o.Gaps) > 0 {
			gaps, err := scrubGaps(o.Gaps)
			if err != nil {
				return nil, err
			}
			o.Gaps = gaps
		}
		rep := &o.Representation
		if rep.State != observedinput.RepPresent {
			// Preserve capture gaps for absent content.
			continue
		}
		reason := ""
		switch {
		case withholdMedia(rep.MediaType):
			reason = "binary_privacy_policy"
		case (origSource.Kind != "file" && origSource.Kind != "url") || strings.TrimSpace(origSource.Locator) == "":
			reason = "unverifiable_source"
		case origSource.Kind == "file":
			switch {
			case !withinRepo(origSource.Locator, repoRoot):
				reason = "unverifiable_source" // outside the repo's ignore scope
			case !ignoreDeterminable:
				reason = "unverifiable_source" // ignore status could not be confirmed
			case ignored[origSource.Locator]:
				reason = "ignored_source"
			}
		}
		if reason != "" {
			// Verify local objects before removing their outbound references.
			if err := verifyContent(rep.ContentRef); err != nil {
				return nil, err
			}
			if err := verifyContent(rep.SourceContentRef); err != nil {
				return nil, err
			}
			rep.ContentRef = ""
			rep.SourceContentRef = ""
			rep.SourceContentSize = 0
			rep.Upload = &observedinput.UploadMarker{State: "withheld", Reason: reason}
			continue
		}
		if rep.ContentRef != "" {
			h, size, err := transformContent(rep.ContentRef)
			if err != nil {
				return nil, err
			}
			rep.ContentRef, rep.ContentSize = h, size
		}
		if rep.SourceContentRef != "" {
			h, size, err := transformContent(rep.SourceContentRef)
			if err != nil {
				return nil, err
			}
			rep.SourceContentRef, rep.SourceContentSize = h, size
		}
	}

	out.RequestLinks = append([]observedinput.RequestInputLink(nil), ev.RequestLinks...)
	for i := range out.RequestLinks {
		out.RequestLinks[i].Source = sanitizeTranscriptSource(out.RequestLinks[i].Source)
	}
	out.ToolCallLinks = append([]observedinput.ToolCallLink(nil), ev.ToolCallLinks...)
	for i := range out.ToolCallLinks {
		out.ToolCallLinks[i].Source = sanitizeTranscriptSource(out.ToolCallLinks[i].Source)
	}
	if len(ev.Gaps) > 0 {
		gaps, err := scrubGaps(ev.Gaps)
		if err != nil {
			return nil, err
		}
		out.Gaps = gaps
	}

	doc, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	// The transform can grow the document; bound the serialized output too.
	if len(doc) > observedInputMaxDocumentBytes {
		return nil, fmt.Errorf("observed-input outbound document %d bytes exceeds limit %d", len(doc), observedInputMaxDocumentBytes)
	}
	up.DocBytes = doc
	up.DocHash = hashHex(doc)
	return up, nil
}

// withholdMedia reports whether a nonempty media type is outside text/*.
func withholdMedia(mediaType string) bool {
	return mediaType != "" && !strings.HasPrefix(mediaType, "text/")
}

// sanitizeTranscriptSource removes the transcript directory, preserving record identity.
func sanitizeTranscriptSource(s observedinput.SourceRef) observedinput.SourceRef {
	if s.Locator != "" {
		s.Locator = filepath.Base(s.Locator)
	}
	return s
}

// sanitizeInputSource shortens file paths and removes secrets from URLs.
func sanitizeInputSource(in observedinput.InputSource, repoRoot string) observedinput.InputSource {
	if in.Locator == "" {
		return in
	}
	switch in.Kind {
	case "url":
		in.Locator = sanitizeURLLocator(in.Locator)
	default:
		in.Locator = repoRelOrBase(in.Locator, repoRoot)
	}
	return in
}

// sanitizeURLLocator removes credentials, queries, fragments, and detected secrets.
// Invalid URLs or redaction failures return an empty locator.
func sanitizeURLLocator(raw string) string {
	if _, err := url.Parse(raw); err != nil {
		return ""
	}
	sanitized := redact.SanitizeURL(raw)
	// Reject URLs that retain credentials, a query, or a fragment.
	if u, err := url.Parse(sanitized); err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	redacted, err := redact.String(sanitized)
	if err != nil {
		return ""
	}
	return redacted
}

// repoRelOrBase returns a repository-relative path or an outside path's basename.
func repoRelOrBase(locator, repoRoot string) string {
	if repoRoot != "" {
		rel, err := filepath.Rel(repoRoot, locator)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return filepath.ToSlash(rel)
		}
	}
	return osIndependentBase(locator)
}

// scrubGaps redacts detected secrets and shortens absolute paths in gap details.
func scrubGaps(gaps []observedinput.Gap) ([]observedinput.Gap, error) {
	out := append([]observedinput.Gap(nil), gaps...)
	for i := range out {
		if out[i].Detail == "" {
			continue
		}
		scrubbed, err := scrubText(out[i].Detail)
		if err != nil {
			return nil, fmt.Errorf("scrub gap detail: %w", err)
		}
		out[i].Detail = scrubbed
	}
	return out, nil
}

// Delimited paths may contain spaces; bare paths end at whitespace.
var (
	dquoteAbsPath  = regexp.MustCompile(`"(?:/|[A-Za-z]:\\)[^"]*"`)
	squoteAbsPath  = regexp.MustCompile(`'(?:/|[A-Za-z]:\\)[^']*'`)
	parenAbsPath   = regexp.MustCompile(`\((?:/|[A-Za-z]:\\)[^)]*\)`)
	posixAbsInText = regexp.MustCompile(`/[^/\s"'()]+(?:/[^/\s"'()]+)+`)
	winAbsInText   = regexp.MustCompile(`[A-Za-z]:(?:\\[^\\\s"'()]+)+`)
)

// scrubText redacts detected secrets and replaces matched absolute paths with basenames.
func scrubText(s string) (string, error) {
	redacted, err := redact.String(s)
	if err != nil {
		return "", err
	}
	for _, re := range []*regexp.Regexp{dquoteAbsPath, squoteAbsPath, parenAbsPath} {
		redacted = re.ReplaceAllStringFunc(redacted, func(m string) string {
			return m[:1] + osIndependentBase(m[1:len(m)-1]) + m[len(m)-1:]
		})
	}
	redacted = posixAbsInText.ReplaceAllStringFunc(redacted, osIndependentBase)
	redacted = winAbsInText.ReplaceAllStringFunc(redacted, osIndependentBase)
	return redacted, nil
}

// osIndependentBase returns the last segment using either path separator.
func osIndependentBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// withinRepo checks lexical containment within repoRoot.
func withinRepo(locator, repoRoot string) bool {
	if repoRoot == "" || locator == "" {
		return false
	}
	rel, err := filepath.Rel(repoRoot, locator)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// gitIgnoreDecision returns ignored paths and whether the result is conclusive.
// Probe or ignore-check failures cause the caller to withhold content.
func gitIgnoreDecision(ctx context.Context, repoRoot string, inRepoPaths []string) (map[string]bool, bool) {
	if len(inRepoPaths) == 0 {
		return nil, true
	}
	isRepo, determinable := gitWorkTreeState(ctx, repoRoot)
	if !determinable {
		return nil, false
	}
	if !isRepo {
		return nil, true // Confirmed outside a Git work tree.
	}
	return checkGitIgnoredStrict(ctx, repoRoot, inRepoPaths)
}

// gitWorkTreeState distinguishes a confirmed non-repository from a failed probe.
func gitWorkTreeState(ctx context.Context, repoRoot string) (isRepo bool, determinable bool) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--is-inside-work-tree")
	platform.HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return false, false // cancelled or timed out
	}
	if err == nil {
		return strings.TrimSpace(stdout.String()) == "true", true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && strings.Contains(stderr.String(), "not a git repository") {
		return false, true // Confirmed non-repository.
	}
	return false, false // Unconfirmed repository state.
}

// checkGitIgnoredStrict returns false on Git errors other than exit 1 (no matches).
func checkGitIgnoredStrict(ctx context.Context, repoRoot string, paths []string) (map[string]bool, bool) {
	var stdin bytes.Buffer
	for _, p := range paths {
		stdin.WriteString(p)
		stdin.WriteByte(0)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "check-ignore", "--stdin", "-z")
	platform.HideWindow(cmd)
	cmd.Stdin = &stdin
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return map[string]bool{}, true // no matches: confirmed clean
		}
		return nil, false // could not determine ignore status
	}
	ignored := map[string]bool{}
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			ignored[p] = true
		}
	}
	return ignored, true
}

// rewriteObservedInputEvidenceHash sets the evidence hash in an existing bundle section.
func rewriteObservedInputEvidenceHash(bundleBytes []byte, uploadHash string) []byte {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(bundleBytes, &generic); err != nil {
		return bundleBytes
	}
	sectionRaw, ok := generic["observed_input"]
	if !ok {
		return bundleBytes
	}
	var section map[string]json.RawMessage
	if json.Unmarshal(sectionRaw, &section) != nil {
		return bundleBytes
	}
	encoded, _ := json.Marshal(uploadHash)
	section["evidence_hash"] = encoded
	rewrittenSection, _ := json.Marshal(section)
	generic["observed_input"] = rewrittenSection
	result, err := json.Marshal(generic)
	if err != nil {
		return bundleBytes
	}
	return result
}

// observedInputEvidenceHashFromBundle returns an available section's hash, or "".
func observedInputEvidenceHashFromBundle(bundleBytes []byte) string {
	var bundle struct {
		ObservedInput *struct {
			EvidenceHash string `json:"evidence_hash"`
			Unavailable  bool   `json:"unavailable"`
		} `json:"observed_input"`
	}
	if json.Unmarshal(bundleBytes, &bundle) != nil || bundle.ObservedInput == nil {
		return ""
	}
	if bundle.ObservedInput.Unavailable {
		return ""
	}
	return bundle.ObservedInput.EvidenceHash
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func hashHex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
