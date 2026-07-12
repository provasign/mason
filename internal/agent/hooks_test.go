package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

func hookSession(t *testing.T, hooks HookSet) *Session {
	t.Helper()
	return New(nil, nil, Options{Root: t.TempDir(), Out: io.Discard, Hooks: hooks})
}

func TestPreBashHookBlocks(t *testing.T) {
	s := hookSession(t, HookSet{"pre_bash": {
		{Match: "git push*", Run: "echo pushes are forbidden here; exit 1", BlockOnFail: true},
	}})
	_, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "git push origin main"}})
	if err == nil || !strings.Contains(err.Error(), "pushes are forbidden") {
		t.Fatalf("blocking hook must refuse with its output, got %v", err)
	}
	// Non-matching commands run untouched.
	out, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "echo ok"}})
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("non-matching command must run: %q %v", out, err)
	}
}

func TestPostEditHookRunsFormatter(t *testing.T) {
	var s *Session
	s = hookSession(t, HookSet{"post_edit": {
		{Match: "*.txt", Run: `printf formatted > "$MASON_FILE"`},
	}})
	f := filepath.Join(s.root, "a.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "edit_file",
		Args: map[string]any{"path": "a.txt", "old_text": "hello", "new_text": "world"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	if string(b) != "formatted" {
		t.Fatalf("post_edit hook must have run on the file, got %q", b)
	}
}

func TestHookFailureBecomesWarning(t *testing.T) {
	s := hookSession(t, HookSet{"post_write": {
		{Run: "echo lint sad; exit 3"}, // no block_on_fail
	}})
	out, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "write_file",
		Args: map[string]any{"path": "b.txt", "content": "x"}})
	if err != nil {
		t.Fatalf("non-blocking hook failure must not fail the tool: %v", err)
	}
	if !strings.Contains(out, "hook warnings") || !strings.Contains(out, "lint sad") {
		t.Fatalf("warning must reach the model: %q", out)
	}
}

func TestLoadHooks(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".mason"), 0o755)
	os.WriteFile(filepath.Join(root, ".mason", "config.json"), []byte(`{
		"hooks": {"pre_bash": [{"match": "rm *", "run": "exit 1", "block_on_fail": true}]}
	}`), 0o644)
	h := LoadHooks(root)
	if len(h["pre_bash"]) != 1 || !h["pre_bash"][0].BlockOnFail {
		t.Fatalf("hooks not loaded: %+v", h)
	}
	if LoadHooks(t.TempDir()) != nil {
		t.Fatal("missing config must load nil hooks")
	}
}
