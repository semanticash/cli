package version

import "testing"

func TestDisplay(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	t.Cleanup(func() {
		Version = origVersion
		Commit = origCommit
	})

	Version = "v0.1.0"
	Commit = "abc1234"
	if got := Display(); got != "v0.1.0 (abc1234)" {
		t.Fatalf("Display() = %q, want %q", got, "v0.1.0 (abc1234)")
	}
}

func TestDisplayWithoutCommit(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	t.Cleanup(func() {
		Version = origVersion
		Commit = origCommit
	})

	Version = "v0.1.0"
	Commit = ""
	if got := Display(); got != "v0.1.0" {
		t.Fatalf("Display() = %q, want %q", got, "v0.1.0")
	}
}
