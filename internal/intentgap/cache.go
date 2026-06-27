package intentgap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AnalysisCacheSchemaVersion identifies the cache file shape. Bumped
// when the on-disk layout changes; readers ignore cache files written
// against a different version and treat them as a miss.
const AnalysisCacheSchemaVersion = "1"

// AnalysisCache is the on-disk snapshot of a completed local analysis.
// It contains everything needed to either render findings to the user
// or upload them later without re-running the analyzer.
//
// A cache entry is reusable only when every key field matches the
// current run. The key covers the code, prompt template, wire schema,
// repository/PR namespace, and diff scope.
type AnalysisCache struct {
	SchemaVersion         string          `json:"schema_version"`
	FindingSchemaVersion  string          `json:"finding_schema_version"`
	HeadSHA               string          `json:"head_sha"`
	BaseSHA               string          `json:"base_sha"`
	RequestedBase         string          `json:"requested_base"`
	PRNumber              int32           `json:"pr_number"`
	RepositoryID          string          `json:"repository_id"`
	PromptTemplateVersion string          `json:"prompt_template_version"`
	Provider              string          `json:"provider"`
	Model                 string          `json:"model"`
	AnalyzedAt            time.Time       `json:"analyzed_at"`
	Findings              json.RawMessage `json:"findings"`
	CoverageSummary       json.RawMessage `json:"coverage_summary"`
}

// AnalysisCacheKey contains the fields that define a reusable analysis.
// BaseSHA is included because the base ref can advance while head_sha
// stays fixed.
type AnalysisCacheKey struct {
	HeadSHA               string
	BaseSHA               string
	PromptTemplateVersion string
	FindingSchemaVersion  string
	RepositoryID          string
	PRNumber              int32
	RequestedBase         string
}

// CacheDir returns the directory where cached analyses are written.
// Callers pass the .semantica directory inside the working repo; the
// cache lives under intent-gap/ to keep it grouped with other intent-
// gap state.
func CacheDir(semDir string) string {
	return filepath.Join(semDir, "intent-gap")
}

// cachePath returns the file path for one head SHA.
func cachePath(semDir, headSHA string) string {
	return filepath.Join(CacheDir(semDir), headSHA+".json")
}

// CacheFileExists reports whether a cache file exists for headSHA.
// It does not validate freshness; callers use ReadAnalysisCache for that.
func CacheFileExists(semDir, headSHA string) bool {
	if headSHA == "" {
		return false
	}
	_, err := os.Stat(cachePath(semDir, headSHA))
	return err == nil
}

// WriteAnalysisCache atomically writes the cache file for one head SHA.
// The parent directory is created on demand. Write-then-rename keeps
// readers from observing a half-written file if the CLI exits mid-write.
func WriteAnalysisCache(semDir string, ca *AnalysisCache) error {
	if ca == nil {
		return fmt.Errorf("nil cache entry")
	}
	if ca.HeadSHA == "" {
		return fmt.Errorf("cache entry missing head_sha")
	}
	if ca.SchemaVersion == "" {
		ca.SchemaVersion = AnalysisCacheSchemaVersion
	}
	dir := CacheDir(semDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.MarshalIndent(ca, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	final := cachePath(semDir, ca.HeadSHA)
	tmp, err := os.CreateTemp(dir, "."+ca.HeadSHA+".json.*")
	if err != nil {
		return fmt.Errorf("create temp cache: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp cache: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename cache: %w", err)
	}
	return nil
}

// ReadAnalysisCache loads the cache file for a head SHA. It returns a
// hit only when every key field matches and cached findings still
// validate. Missing files, mismatches, corrupt JSON, and schema-invalid
// findings are clean misses. Other read errors are returned.
func ReadAnalysisCache(semDir string, key AnalysisCacheKey) (*AnalysisCache, bool, error) {
	if key.HeadSHA == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(cachePath(semDir, key.HeadSHA))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read cache: %w", err)
	}
	var ca AnalysisCache
	if err := json.Unmarshal(data, &ca); err != nil {
		return nil, false, nil
	}
	if ca.SchemaVersion != AnalysisCacheSchemaVersion ||
		ca.FindingSchemaVersion != key.FindingSchemaVersion ||
		ca.HeadSHA != key.HeadSHA ||
		ca.BaseSHA != key.BaseSHA ||
		ca.PromptTemplateVersion != key.PromptTemplateVersion ||
		ca.RepositoryID != key.RepositoryID ||
		ca.PRNumber != key.PRNumber ||
		ca.RequestedBase != key.RequestedBase {
		return nil, false, nil
	}
	// Cached findings must still satisfy the current schema.
	if err := ValidateFindings(ca.Findings); err != nil {
		return nil, false, nil
	}
	return &ca, true, nil
}
