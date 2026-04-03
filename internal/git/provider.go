package git

import (
	"net/url"
	"strings"
)

// ProviderFromRemoteURL returns the hosting provider based on the remote URL hostname.
// Returns "github", "gitlab", or "unknown".
func ProviderFromRemoteURL(remoteURL string) string {
	host := extractHost(remoteURL)
	switch strings.ToLower(host) {
	case "github.com":
		return "github"
	case "gitlab.com":
		return "gitlab"
	default:
		return "unknown"
	}
}

// extractHost parses the hostname from a remote URL.
// Handles HTTPS (https://github.com/...), SSH (git@github.com:...),
// and ssh:// (ssh://git@github.com/...) formats.
func extractHost(remoteURL string) string {
	remoteURL = strings.TrimSpace(remoteURL)

	// SSH shorthand: git@github.com:org/repo.git
	if strings.Contains(remoteURL, "@") && strings.Contains(remoteURL, ":") && !strings.Contains(remoteURL, "://") {
		// Extract between @ and :
		atIdx := strings.Index(remoteURL, "@")
		colonIdx := strings.Index(remoteURL[atIdx:], ":")
		if colonIdx > 0 {
			return remoteURL[atIdx+1 : atIdx+colonIdx]
		}
	}

	// Standard URL: https://github.com/... or ssh://git@github.com/...
	u, err := url.Parse(remoteURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host != "" {
		return host
	}

	return ""
}
