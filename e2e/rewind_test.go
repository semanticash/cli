//go:build e2e

package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRewindRestoresFiles(t *testing.T) {
	dir, env := initGitRepo(t)
	enableRepo(t, env, dir)

	// Commit v1 of app.go.
	v1Content := "package main\n\nfunc main() { println(\"v1\") }\n"
	hash1 := commitFile(t, env, dir, "app.go", v1Content, "add app.go v1")
	runWorker(t, env, dir, hash1)

	// Record the checkpoint for v1.
	cp1 := latestCheckpointID(t, env, dir)

	// Commit v2 of app.go.
	v2Content := "package main\n\nfunc main() { println(\"v2\") }\n"
	hash2 := commitFile(t, env, dir, "app.go", v2Content, "update app.go v2")
	runWorker(t, env, dir, hash2)

	// Rewind to cp1.
	rewindOut := runSem(t, env, dir, "rewind", cp1, "--json")
	var rw rewindOutput
	if err := json.Unmarshal([]byte(rewindOut), &rw); err != nil {
		t.Fatalf("parse rewind output: %v\n%s", err, rewindOut)
	}

	if rw.SafetyCheckpointID == "" {
		t.Error("safety_checkpoint_id is empty")
	}
	if rw.FilesRestored == 0 {
		t.Error("files_restored = 0, want > 0")
	}

	// Verify app.go contains v1 content.
	data, err := os.ReadFile(filepath.Join(dir, "app.go"))
	if err != nil {
		t.Fatalf("read app.go: %v", err)
	}
	if string(data) != v1Content {
		t.Errorf("app.go content = %q, want v1", string(data))
	}

	// Safety checkpoint should appear in list.
	cps := listCheckpoints(t, env, dir)
	foundSafety := false
	for _, cp := range cps.Items {
		if cp.ID == rw.SafetyCheckpointID {
			foundSafety = true
			break
		}
	}
	if !foundSafety {
		t.Error("safety checkpoint not found in list")
	}
}

func TestRewindExactDeletesExtraFiles(t *testing.T) {
	dir, env := initGitRepo(t)
	enableRepo(t, env, dir)

	// Commit app.go.
	hash1 := commitFile(t, env, dir, "app.go",
		"package main\n\nfunc main() {}\n", "add app.go")
	runWorker(t, env, dir, hash1)
	cp1 := latestCheckpointID(t, env, dir)

	// Commit extra.go.
	hash2 := commitFile(t, env, dir, "extra.go",
		"package main\n\nfunc extra() {}\n", "add extra.go")
	runWorker(t, env, dir, hash2)

	// Rewind to cp1 with --exact.
	rewindOut := runSem(t, env, dir, "rewind", cp1, "--exact", "--json")
	var rw rewindOutput
	if err := json.Unmarshal([]byte(rewindOut), &rw); err != nil {
		t.Fatalf("parse rewind output: %v\n%s", err, rewindOut)
	}

	// extra.go should be deleted.
	if _, err := os.Stat(filepath.Join(dir, "extra.go")); !os.IsNotExist(err) {
		t.Error("extra.go should not exist after --exact rewind")
	}
	if rw.FilesDeleted == 0 {
		t.Error("files_deleted = 0, want > 0")
	}
}
