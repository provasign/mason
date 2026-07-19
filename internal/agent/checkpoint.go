package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Checkpointing: before every task that might change files, the working
// tree (tracked + untracked, gitignore respected) is snapshotted as an
// unreferenced git commit object — nothing in the user's index, HEAD, or
// reflog moves. /undo restores the tree to the snapshot taken before the
// last mutating task.

// snapshotTree writes the current tree state to a commit object via a
// TEMPORARY index (the user's staging area is never touched) and returns
// its hash. "" when git is unavailable.
func snapshotTree(root string) string {
	tmp, err := os.CreateTemp("", "mason-index-*")
	if err != nil {
		return ""
	}
	tmpIndex := tmp.Name()
	tmp.Close()
	os.Remove(tmpIndex) // git wants a valid index or NO file — not an empty one
	defer os.Remove(tmpIndex)

	env := append(os.Environ(), "GIT_INDEX_FILE="+tmpIndex)
	run := func(args ...string) (string, error) {
		// Byte-exact snapshots: autocrlf must not rewrite content in either
		// direction (Windows runners default it on).
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "core.autocrlf=false", "-c", "core.safecrlf=false"}, args...)...)
		cmd.Env = env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	// A checkpoint is an unreachable Git commit and therefore persists until
	// object pruning. Refuse it entirely when an untracked credential-bearing
	// path would be captured by `git add -A`; a partial snapshot would make
	// undo destructive for the excluded file. (checkpoint() warns the user;
	// this guard also protects any direct caller.)
	if len(sensitiveCheckpointPaths(root)) > 0 {
		return ""
	}
	// Seed the temp index from HEAD when it exists so deletions are captured.
	if _, err := run("rev-parse", "HEAD"); err == nil {
		if _, err := run("read-tree", "HEAD"); err != nil {
			return ""
		}
	}
	if _, err := run("add", "-A", "."); err != nil {
		return ""
	}
	tree, err := run("write-tree")
	if err != nil {
		return ""
	}
	args := []string{"-c", "core.autocrlf=false", "commit-tree", tree, "-m", "mason checkpoint"}
	if head, err := run("rev-parse", "HEAD"); err == nil {
		args = append(args, "-p", head)
	}
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(env,
		"GIT_AUTHOR_NAME=mason", "GIT_AUTHOR_EMAIL=mason@local",
		"GIT_COMMITTER_NAME=mason", "GIT_COMMITTER_EMAIL=mason@local")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sensitiveCheckpointPaths returns untracked, non-ignored credential-bearing
// files that a checkpoint's `git add -A` would otherwise write into a Git
// object. Non-empty means the checkpoint must be refused. Tracked files are
// already in history, so only untracked paths are a new exposure.
func sensitiveCheckpointPaths(root string) []string {
	cmd := exec.Command("git", "-C", root, "ls-files", "-o", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var hits []string
	for _, path := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if checkpointSensitivePath(path) {
			hits = append(hits, path)
		}
	}
	return hits
}

func checkpointSensitivePath(path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		switch strings.ToLower(part) {
		case ".ssh", ".aws", ".gnupg", ".kube":
			return true
		}
	}
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.HasPrefix(base, ".env.") ||
		base == "credentials.json" || strings.HasPrefix(base, "secrets.") {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".key", ".pem", ".p12", ".pfx", ".jks", ".keystore", ".pkcs12":
		return true
	}
	return false
}

// restoreSnapshot returns the working tree to the snapshot state: files in
// the snapshot are written back, files created since (tracked or untracked,
// non-ignored) are removed. The user's index and HEAD are untouched.
func restoreSnapshot(root, snap string) error {
	tmp, err := os.CreateTemp("", "mason-index-*")
	if err != nil {
		return err
	}
	tmpIndex := tmp.Name()
	tmp.Close()
	os.Remove(tmpIndex) // git wants a valid index or NO file — not an empty one
	defer os.Remove(tmpIndex)
	env := append(os.Environ(), "GIT_INDEX_FILE="+tmpIndex)

	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "core.autocrlf=false", "-c", "core.safecrlf=false"}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	// Current non-ignored files (tracked + untracked).
	nowOut, err := git("ls-files", "-co", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("ls-files: %s", nowOut)
	}
	// Files in the snapshot.
	snapOut, err := git("ls-tree", "-r", "--name-only", snap)
	if err != nil {
		return fmt.Errorf("ls-tree: %s", snapOut)
	}
	inSnap := map[string]bool{}
	for _, f := range strings.Split(snapOut, "\n") {
		if f != "" {
			inSnap[f] = true
		}
	}
	// Write snapshot content into the worktree.
	if out, err := git("read-tree", snap); err != nil {
		return fmt.Errorf("read-tree: %s", out)
	}
	if out, err := git("checkout-index", "-a", "-f"); err != nil {
		return fmt.Errorf("checkout-index: %s", out)
	}
	// Remove files that did not exist at snapshot time.
	for _, f := range strings.Split(nowOut, "\n") {
		if f == "" || inSnap[f] {
			continue
		}
		_ = os.Remove(filepath.Join(root, f))
	}
	return nil
}

// checkpoint records a snapshot at task start (kept on a small stack).
func (s *Session) checkpoint() {
	if hits := sensitiveCheckpointPaths(s.root); len(hits) > 0 {
		fmt.Fprintf(s.out, "  %s\n", s.st.dim(fmt.Sprintf(
			"checkpoint skipped — %s could expose credentials; /undo is unavailable for this task",
			hits[0])))
		return
	}
	snap := snapshotTree(s.root)
	if snap == "" {
		return
	}
	s.checkpoints = append(s.checkpoints, snap)
	if len(s.checkpoints) > 10 {
		s.checkpoints = s.checkpoints[len(s.checkpoints)-10:]
	}
}

// Undo restores the tree to the state before the most recent task and pops
// that checkpoint. Returns a human-readable result.
func (s *Session) Undo() (string, error) {
	if len(s.checkpoints) == 0 {
		return "", fmt.Errorf("nothing to undo — no checkpoints in this session")
	}
	snap := s.checkpoints[len(s.checkpoints)-1]
	s.checkpoints = s.checkpoints[:len(s.checkpoints)-1]
	if err := restoreSnapshot(s.root, snap); err != nil {
		return "", err
	}
	// The graph must follow the tree.
	if s.invoke != nil {
		_, _ = s.invoke("prism_index", map[string]any{})
	}
	// Folded into the NEXT task's message (never appended directly: two
	// consecutive user messages break the Anthropic API).
	s.pendingNote = "[the user reverted the previous task's file changes with /undo — the working tree no longer contains them]"
	return "working tree restored to the state before the last task (" + snap[:8] + ")", nil
}
