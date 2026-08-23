package carryforward

// IdentifyCreatedCandidates returns created paths that were already present
// in the previous checkpoint manifest. That means the commit is adding a file
// the checkpoint had already seen on disk, so older events may apply.
func IdentifyCreatedCandidates(filesCreated []string, manifestFiles []ManifestEntry) map[string]bool {
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
