package agent

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

func TestScanQualityFindsPlaceholders(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tests"), 0o755)
	os.WriteFile(filepath.Join(dir, "tests", "test_main.py"), []byte(
		"import unittest\n\nclass T(unittest.TestCase):\n    def test_main(self):\n        self.assertTrue(True)\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "impl.py"), []byte(
		"def collect(data):\n    raise NotImplementedError\n\ndef helper():\n    pass\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "real.py"), []byte(
		"def add(a, b):\n    return a + b\n"), 0o644)

	findings := scanQuality(dir, []string{"tests/test_main.py", "impl.py", "real.py"})
	var got []string
	for _, f := range findings {
		got = append(got, f.String())
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "test_main.py:5") || !strings.Contains(joined, "placeholder test") {
		t.Fatalf("assertTrue(True) not flagged: %v", got)
	}
	if !strings.Contains(joined, "impl.py:2") {
		t.Fatalf("NotImplementedError not flagged: %v", got)
	}
	if !strings.Contains(joined, "impl.py:3") && !strings.Contains(joined, "impl.py:4") {
		t.Fatalf("pass-only function not flagged: %v", got)
	}
	for _, g := range got {
		if strings.Contains(g, "real.py") {
			t.Fatalf("real code falsely flagged: %v", got)
		}
	}
}

func TestScanQualityCleanCode(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte(
		"package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"), 0o644)
	if f := scanQuality(dir, []string{"calc_test.go"}); len(f) != 0 {
		t.Fatalf("clean test flagged: %v", f)
	}
}

// The gate must bounce a task that shipped a placeholder test without
// running anything, and accept after the model fixes and runs the suite.
func TestQualityGateBouncesPlaceholders(t *testing.T) {
	dir := t.TempDir()
	// git repo so the changed-file diff works
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if err := cmd.Run(); err != nil {
			t.Skip("git unavailable")
		}
	}
	fp := &fakeProvider{replies: []provider.Msg{
		// writes a vacuous test, claims done without running it
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "write_file",
			Args: map[string]any{"path": "test_thing.py",
				"content": "def test_thing():\n    assert True\n"}}}},
		{Role: "assistant", Content: "Added tests, all done."},
		// after the bounce: writes a real test and runs it
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "2", Name: "write_file",
			Args: map[string]any{"path": "test_thing.py",
				"content": "from thing import add\n\ndef test_thing():\n    assert add(2, 3) == 5\n"}}}},
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "3", Name: "bash",
			Args: map[string]any{"command": "echo pytest simulated: 1 passed"}}}},
		{Role: "assistant", Content: "Real test added and suite executed."},
	}}
	s := New(fp, nil, Options{Root: dir, Out: io.Discard})
	reply, err := s.Ask(context.Background(), "add tests for the thing module")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "suite executed") {
		t.Fatalf("placeholder work was accepted: %q", reply)
	}
	// The bounce message must carry the concrete finding.
	sawGate := false
	for _, m := range s.History() {
		if m.Role == "user" && strings.Contains(m.Content, "Quality gate") &&
			strings.Contains(m.Content, "test_thing.py") {
			sawGate = true
		}
	}
	if !sawGate {
		t.Fatal("gate bounce with file:line findings not sent to the model")
	}
}

// A task with real tests that were actually run must pass the gate silently.
func TestQualityGatePassesRealWork(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if err := cmd.Run(); err != nil {
			t.Skip("git unavailable")
		}
	}
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "write_file",
			Args: map[string]any{"path": "add_test.go",
				"content": "package p\n\nfunc TestAdd(t *testing.T) { if Add(1,2)!=3 { t.Fatal() } }\n"}}}},
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "2", Name: "bash",
			Args: map[string]any{"command": "echo go test ./... ok"}}}},
		{Role: "assistant", Content: "done, tests green"},
	}}
	s := New(fp, nil, Options{Root: dir, Out: io.Discard})
	reply, err := s.Ask(context.Background(), "add a test for Add")
	if err != nil || reply != "done, tests green" {
		t.Fatalf("real work bounced: %q %v", reply, err)
	}
	if len(fp.turns) != 3 {
		t.Fatalf("unexpected extra turns (gate false-fired): %d", len(fp.turns))
	}
}

// Edits inside UNTRACKED files must move the fingerprint and appear in the
// changed set — git status collapses untracked dirs and git diff ignores
// untracked content, which blinded the guards in fresh projects.
func TestUntrackedEditVisibility(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if err := cmd.Run(); err != nil {
			t.Skip("git unavailable")
		}
	}
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "m.py"), []byte("v1\n"), 0o644)
	fp0 := treeFingerprint(dir)
	st0 := porcelainStatus(dir)
	// edit the already-untracked file (different size so the sig must move
	// even on filesystems with coarse mtimes)
	os.WriteFile(filepath.Join(dir, "src", "m.py"), []byte("v2 changed\n"), 0o644)
	if treeFingerprint(dir) == fp0 {
		t.Fatal("fingerprint blind to untracked-file edit")
	}
	changed := changedFilesSince(dir, st0)
	found := false
	for _, f := range changed {
		if f == "src/m.py" {
			found = true
		}
	}
	if !found {
		t.Fatalf("untracked edit missing from changed set: %v", changed)
	}
}
