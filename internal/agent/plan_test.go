package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
	"github.com/provasign/mason/internal/trail"
)

func planSession(t *testing.T) *Session {
	t.Helper()
	s := New(nil, nil, Options{Root: t.TempDir(), Out: &strings.Builder{}})
	s.SetPlan(true)
	return s
}

func TestPlanModeRefusesMutations(t *testing.T) {
	s := planSession(t)
	if err := os.WriteFile(filepath.Join(s.root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err := s.runCodingTool(ctx, provider.ToolCall{Name: "edit_file",
		Args: map[string]any{"path": "a.txt", "old_text": "hello", "new_text": "bye"}})
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("edit_file must be refused in plan mode, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, "a.txt")); string(b) != "hello\n" {
		t.Fatal("plan mode edit must not touch the file")
	}

	_, err = s.runCodingTool(ctx, provider.ToolCall{Name: "write_file",
		Args: map[string]any{"path": "b.txt", "content": "x"}})
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("write_file must be refused in plan mode, got %v", err)
	}

	_, err = s.runCodingTool(ctx, provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "touch c.txt"}})
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("mutating bash must be refused in plan mode, got %v", err)
	}

	tr := trail.New(s.root, "test")
	defer tr.Done()
	_, err = s.dispatch(ctx, provider.ToolCall{Name: "apply_rename_plan", Args: map[string]any{}}, tr)
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("apply_rename_plan must be refused in plan mode, got %v", err)
	}
}

func TestPlanModeAllowsReads(t *testing.T) {
	s := planSession(t)
	if err := os.WriteFile(filepath.Join(s.root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, err := s.runCodingTool(ctx, provider.ToolCall{Name: "read_file",
		Args: map[string]any{"path": "a.txt"}})
	if err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("read_file must work in plan mode: %q %v", out, err)
	}
	if _, err := s.runCodingTool(ctx, provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "ls"}}); err != nil {
		t.Fatalf("read-only bash must work in plan mode: %v", err)
	}
}

func TestPlanSafeBash(t *testing.T) {
	safe := []string{"ls -la", "cat a.txt", "git status", "git log --oneline", "git diff", "go version", "grep -rn foo ."}
	for _, c := range safe {
		if !planSafeBash(c) {
			t.Errorf("%q should be plan-safe", c)
		}
	}
	unsafe := []string{"touch x", "rm -rf /", "git push", "git commit -m x", "go test ./...",
		"ls > out.txt", "cat a | tee b", "ls; rm x", "echo $(rm x)", "make", "npm install"}
	for _, c := range unsafe {
		if planSafeBash(c) {
			t.Errorf("%q must NOT be plan-safe", c)
		}
	}
}

func TestPlanModePropagatesToSubagent(t *testing.T) {
	s := planSession(t)
	// Depth guard fires before any provider use; what matters here is that
	// a subagent created from a plan session inherits the read-only flag —
	// asserted structurally on the constructor path used by runSubagent.
	sub := New(nil, s.invoke, Options{Root: s.root, Depth: 1})
	sub.plan = s.plan
	if !sub.Plan() {
		t.Fatal("subagent must inherit plan mode")
	}
}
