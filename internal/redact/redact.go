package redact

import (
	"net/url"
	"sort"
	"strings"

	"github.com/zricethezav/gitleaks/v8/report"
)

const replacementToken = "[REDACTED]"

// String redacts detected secrets in s and returns the original string when
// no findings are present.
func String(s string) (string, error) {
	if s == "" {
		return s, nil
	}
	findings, err := scan(s)
	if err != nil {
		return "", err
	}
	if len(findings) == 0 {
		return s, nil
	}
	return applyRedactions(s, findings), nil
}

// Bytes is a convenience wrapper over String.
// It returns the original slice on the no-op path.
func Bytes(b []byte) ([]byte, error) {
	s := string(b)
	redacted, err := String(s)
	if err != nil {
		return nil, err
	}
	if redacted == s {
		return b, nil
	}
	return []byte(redacted), nil
}

// SanitizeURL strips embedded credentials (userinfo), query strings, and
// fragments from a URL while preserving the scheme, host, and path for
// identity matching. Non-URL strings and SSH-style URLs pass through unchanged.
func SanitizeURL(rawURL string) string {
	// SSH URLs are not parsed by url.Parse in a useful way.
	if strings.HasPrefix(rawURL, "git@") {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		return rawURL
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// span represents a byte range [start, end) in a string.
type span struct {
	start int
	end   int
}

// applyRedactions replaces all matched spans with the redaction token.
func applyRedactions(s string, findings []report.Finding) string {
	spans := collectSpans(s, findings)
	if len(spans) == 0 {
		return s
	}
	spans = mergeSpans(spans)

	// Build the result in a single forward pass.
	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	for _, sp := range spans {
		b.WriteString(s[prev:sp.start])
		b.WriteString(replacementToken)
		prev = sp.end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// collectSpans locates each finding's secret in the original string.
func collectSpans(s string, findings []report.Finding) []span {
	// Deduplicate secrets so we don't scan for the same string twice.
	seen := make(map[string]bool, len(findings))
	var spans []span

	for _, f := range findings {
		secret := f.Secret
		if secret == "" || seen[secret] {
			continue
		}
		seen[secret] = true

		// Redact repeated occurrences of the same secret as well.
		offset := 0
		for {
			idx := strings.Index(s[offset:], secret)
			if idx < 0 {
				break
			}
			start := offset + idx
			spans = append(spans, span{start: start, end: start + len(secret)})
			offset = start + len(secret)
		}
	}
	return spans
}

// mergeSpans sorts spans by start offset and merges overlapping/adjacent ones.
func mergeSpans(spans []span) []span {
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].start < spans[j].start
	})
	merged := []span{spans[0]}
	for _, sp := range spans[1:] {
		last := &merged[len(merged)-1]
		if sp.start <= last.end {
			if sp.end > last.end {
				last.end = sp.end
			}
		} else {
			merged = append(merged, sp)
		}
	}
	return merged
}
