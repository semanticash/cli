package git

import "testing"

func TestProviderFromRemoteURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		// GitHub
		{"https://github.com/org/repo", "github"},
		{"https://github.com/org/repo.git", "github"},
		{"git@github.com:org/repo.git", "github"},
		{"ssh://git@github.com/org/repo", "github"},
		{"https://GITHUB.COM/org/repo", "github"},

		// GitLab
		{"https://gitlab.com/group/project", "gitlab"},
		{"https://gitlab.com/group/project.git", "gitlab"},
		{"git@gitlab.com:group/project.git", "gitlab"},
		{"https://GITLAB.COM/group/project", "gitlab"},

		// Unknown
		{"https://bitbucket.org/org/repo", "unknown"},
		{"https://my-server.example.com/repo", "unknown"},
		{"", "unknown"},

		// Different hosts must not match by substring.
		{"https://notgithub.com/org/repo", "unknown"},
		{"https://github.com.evil.com/org/repo", "unknown"},
		{"https://fakegitlab.com/group/project", "unknown"},
		{"git@notgithub.com:org/repo.git", "unknown"},

		// Self-hosted instances are unknown for now.
		{"https://gitlab.mycompany.com/group/project", "unknown"},
		{"git@github.mycompany.com:org/repo.git", "unknown"},
	}

	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			got := ProviderFromRemoteURL(c.url)
			if got != c.want {
				t.Errorf("ProviderFromRemoteURL(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}
