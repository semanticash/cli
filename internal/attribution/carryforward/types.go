// Package carryforward determines which files are eligible for historical
// lookback during attribution. It is a pure domain package with no
// infrastructure dependencies.
package carryforward

// ManifestEntry holds the path of a file present in a checkpoint manifest.
type ManifestEntry struct {
	Path string
}
