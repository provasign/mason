package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A file that is ALREADY modified when the task starts must still register as
// changed when the agent edits it again. Its porcelain line (" M path") is
// identical before and after, so a line-only signature made the honesty guard
// and the quality gate blind on exactly the files a mid-work session touches.
func TestChangedFilesSeesAlreadyDirtyTrackedFile(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("add", "a.go")
	git("commit", "-qm", "init")

	// Dirty BEFORE the task starts.
	if err := os.WriteFile(path, []byte("package p\n// user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := porcelainStatus(root)
	if len(start) == 0 {
		t.Fatal("expected a dirty working tree at the snapshot")
	}

	// The agent edits the same already-dirty file.
	time.Sleep(10 * time.Millisecond) // distinct mtime
	if err := os.WriteFile(path, []byte("package p\n// user edit\n// agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := changedFilesSince(root, start)
	found := false
	for _, c := range changed {
		if c == "a.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("agent edit to an already-dirty file went unseen; changed = %v", changed)
	}
}
