package version

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "with_v_prefix", in: "v0.1.2", want: "v0.1.2", ok: true},
		{name: "without_v_prefix", in: "0.1.2", want: "v0.1.2", ok: true},
		{name: "release_candidate", in: "v0.1.2-rc.1", want: "v0.1.2-rc.1", ok: true},
		{name: "invalid", in: "dev", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeVersion(tt.in)
			if tt.ok && err != nil {
				t.Fatalf("normalizeVersion(%q) returned error: %v", tt.in, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("normalizeVersion(%q) expected error", tt.in)
			}
			if tt.ok && got != tt.want {
				t.Fatalf("normalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildUpdateInfo(t *testing.T) {
	info, err := buildUpdateInfo("v0.1.0", "v0.2.0", "https://example.com/release")
	if err != nil {
		t.Fatalf("buildUpdateInfo returned error: %v", err)
	}
	if !info.Available {
		t.Fatal("expected update to be available")
	}
	if info.LatestVersion != "v0.2.0" {
		t.Fatalf("latest_version = %q, want v0.2.0", info.LatestVersion)
	}
}

func TestCheckForUpdateUsesFreshCache(t *testing.T) {
	origVersion := Version
	origClient := releaseHTTPClient
	t.Cleanup(func() {
		Version = origVersion
		releaseHTTPClient = origClient
	})

	Version = "v0.1.0"
	releaseHTTPClient = &http.Client{Timeout: 100 * time.Millisecond}

	cacheDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cacheDir)

	cachePath := filepath.Join(cacheDir, "semantica", "release-check.json")
	if err := writeReleaseCache(cachePath, releaseCache{
		SchemaVersion: 1,
		CheckedAt:     time.Now().Unix(),
		LatestVersion: "v0.2.0",
		DownloadURL:   "https://example.com/releases/v0.2.0",
	}); err != nil {
		t.Fatalf("writeReleaseCache: %v", err)
	}

	info, err := CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate returned error: %v", err)
	}
	if info == nil || !info.Available {
		t.Fatalf("expected cached update info, got %#v", info)
	}
}

func TestReadReleaseCacheRejectsUnknownSchema(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "release-check.json")
	if err := writeReleaseCache(cachePath, releaseCache{
		SchemaVersion: 0,
		CheckedAt:     time.Now().Unix(),
		LatestVersion: "v0.2.0",
	}); err != nil {
		t.Fatalf("writeReleaseCache: %v", err)
	}

	if _, err := readReleaseCache(cachePath); err == nil {
		t.Fatal("expected schema mismatch error")
	}
}

func TestCheckForUpdateFetchesLatestRelease(t *testing.T) {
	origVersion := Version
	origClient := releaseHTTPClient
	t.Cleanup(func() {
		Version = origVersion
		releaseHTTPClient = origClient
	})

	Version = "v0.1.0"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/semanticash/cli/releases/latest" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(latestReleaseResponse{
			TagName: "v0.2.0",
			HTMLURL: "https://github.com/semanticash/cli/releases/tag/v0.2.0",
		})
	}))
	defer srv.Close()

	releaseHTTPClient = srv.Client()
	cacheDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cacheDir)

	origURL := releaseAPIURL
	releaseAPIURL = srv.URL + "/repos/semanticash/cli/releases/latest"
	t.Cleanup(func() {
		releaseAPIURL = origURL
	})

	info, err := CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate returned error: %v", err)
	}
	if info == nil || !info.Available {
		t.Fatalf("expected update info, got %#v", info)
	}
}
