package handoff

// LookPathForTest returns the package-level lookPath stub. Used by
// command-level tests in another package that need to swap in a
// deterministic binary detector. Not part of the public API; only
// intended for tests.
func LookPathForTest() func(string) (string, error) { return lookPath }

// SetLookPathForTest replaces the package-level lookPath stub.
// Pair with LookPathForTest to capture the original and restore
// in t.Cleanup.
func SetLookPathForTest(fn func(string) (string, error)) { lookPath = fn }
