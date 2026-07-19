package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if err := cmd.Run(); err != nil {
			t.Skip("git unavailable")
		}
	}
	return dir
}

// Undo must restore modified files (tracked AND untracked) and remove files
// created after the checkpoint — leaving the user's HEAD/index untouched.
func TestCheckpointUndo(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "untracked.py"), []byte("original\n"), 0o644)
	s := New(&fakeProvider{}, nil, Options{Root: dir, Out: nil})
	s.checkpoint()
	// the "task": modifies the untracked file and creates a new one
	os.WriteFile(filepath.Join(dir, "untracked.py"), []byte("MODIFIED\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "created.py"), []byte("new\n"), 0o644)

	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "untracked.py"))
	if string(got) != "original\n" {
		t.Fatalf("modified file not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "created.py")); err == nil {
		t.Fatal("created file not removed by undo")
	}
	// second undo with empty stack must error cleanly
	if _, err := s.Undo(); err == nil {
		t.Fatal("empty undo stack must error")
	}
}

func TestUndoOutsideGit(t *testing.T) {
	s := New(&fakeProvider{}, nil, Options{Root: t.TempDir(), Out: nil})
	s.checkpoint() // no-op outside git
	if _, err := s.Undo(); err == nil {
		t.Fatal("undo outside git must error, not corrupt")
	}
}

func TestCheckpointRefusesSensitiveUntrackedFiles(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN=secret\n"), 0o600)
	if got := snapshotTree(dir); got != "" {
		t.Fatalf("checkpoint captured a sensitive path: %s", got)
	}
}
