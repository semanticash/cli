package carryforward

// IdentifyCandidates returns files eligible for historical lookback:
// created in the current diff AND present in the previous checkpoint's manifest.
func IdentifyCandidates(filesCreated []string, manifestFiles []ManifestEntry) map[string]bool {
	if len(filesCreated) == 0 || len(manifestFiles) == 0 {
		return nil
	}

	manifestSet := make(map[string]bool, len(manifestFiles))
	for _, mf := range manifestFiles {
		manifestSet[mf.Path] = true
	}

	var result map[string]bool
	for _, path := range filesCreated {
		if manifestSet[path] {
			if result == nil {
				result = make(map[string]bool)
			}
			result[path] = true
		}
	}
	return result
}
