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

// IdentifyModifiedCandidates returns modified paths eligible for historical
// lookback. Modified files need no manifest gate; the scorer later requires
// historical lines to match the current diff before crediting them.
func IdentifyModifiedCandidates(filesEdited []string) map[string]bool {
	if len(filesEdited) == 0 {
		return nil
	}
	result := make(map[string]bool, len(filesEdited))
	for _, path := range filesEdited {
		result[path] = true
	}
	return result
}
