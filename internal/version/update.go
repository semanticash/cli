package version

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
	"github.com/semanticash/cli/internal/util"
)

const (
	releaseCheckTTL = 12 * time.Hour
)

var releaseHTTPClient = &http.Client{
	Timeout: 1500 * time.Millisecond,
}

var releaseAPIURL = "https://api.github.com/repos/semanticash/cli/releases/latest"

type UpdateInfo struct {
	CurrentVersion string `json:"current_version,omitempty"`
	LatestVersion  string `json:"latest_version,omitempty"`
	DownloadURL    string `json:"download_url,omitempty"`
	Available      bool   `json:"available"`
}

type releaseCache struct {
	SchemaVersion int    `json:"schema_version"`
	CheckedAt     int64  `json:"checked_at"`
	LatestVersion string `json:"latest_version"`
	DownloadURL   string `json:"download_url,omitempty"`
}

type latestReleaseResponse struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// CheckForUpdate returns the latest published CLI release when it is newer
// than the current binary version. The release metadata is cached in the
// global Semantica config directory to keep local commands fast and usable
// while offline.
func CheckForUpdate(ctx context.Context) (*UpdateInfo, error) {
	currentVersion, ok := normalizedCurrentVersion()
	if !ok {
		return nil, nil
	}

	cachePath, err := releaseCachePath()
	if err != nil {
		return nil, err
	}

	cached, _ := readReleaseCache(cachePath)
	if cached != nil && time.Since(time.Unix(cached.CheckedAt, 0)) < releaseCheckTTL {
		return buildUpdateInfo(currentVersion, cached.LatestVersion, cached.DownloadURL)
	}

	latestVersion, downloadURL, fetchErr := fetchLatestRelease(ctx)
	if fetchErr == nil {
		_ = writeReleaseCache(cachePath, releaseCache{
			SchemaVersion: 1,
			CheckedAt:     time.Now().Unix(),
			LatestVersion: latestVersion,
			DownloadURL:   downloadURL,
		})
		return buildUpdateInfo(currentVersion, latestVersion, downloadURL)
	}

	if cached != nil {
		return buildUpdateInfo(currentVersion, cached.LatestVersion, cached.DownloadURL)
	}

	return nil, fetchErr
}

func normalizedCurrentVersion() (string, bool) {
	v := strings.TrimSpace(Version)
	if v == "" || v == "dev" {
		return "", false
	}

	normalized, err := normalizeVersion(v)
	if err != nil {
		return "", false
	}
	return normalized, true
}

func buildUpdateInfo(currentVersion, latestVersion, downloadURL string) (*UpdateInfo, error) {
	if currentVersion == "" || latestVersion == "" {
		return nil, nil
	}

	currentParsed, err := semver.NewVersion(strings.TrimPrefix(currentVersion, "v"))
	if err != nil {
		return nil, err
	}
	latestParsed, err := semver.NewVersion(strings.TrimPrefix(latestVersion, "v"))
	if err != nil {
		return nil, err
	}

	return &UpdateInfo{
		CurrentVersion: currentVersion,
		LatestVersion:  latestVersion,
		DownloadURL:    downloadURL,
		Available:      latestParsed.GreaterThan(currentParsed),
	}, nil
}

func fetchLatestRelease(ctx context.Context) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPIURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := releaseHTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("release check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("release check returned status %d", resp.StatusCode)
	}

	var payload latestReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", fmt.Errorf("decode release check response: %w", err)
	}

	version, err := normalizeVersion(payload.TagName)
	if err != nil {
		return "", "", err
	}

	return version, strings.TrimSpace(payload.HTMLURL), nil
}

// releaseCachePath stores the release check cache alongside other global
// Semantica config files under ~/.config/semantica or XDG_CONFIG_HOME.
func releaseCachePath() (string, error) {
	configRoot, err := util.AppConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configRoot, "release-check.json"), nil
}

func readReleaseCache(path string) (*releaseCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cached releaseCache
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}
	if cached.SchemaVersion != 1 {
		return nil, errors.New("unsupported release cache format")
	}
	return &cached, nil
}

func writeReleaseCache(path string, cached releaseCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cached, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func normalizeVersion(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", errors.New("empty version")
	}
	v = strings.TrimPrefix(v, "v")

	parsed, err := semver.NewVersion(v)
	if err != nil {
		return "", err
	}

	return "v" + parsed.String(), nil
}
