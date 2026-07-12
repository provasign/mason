package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

func TestSelfRevertGuard(t *testing.T) {
	s := New(nil, nil, Options{Root: t.TempDir(), Out: io.Discard})
	s.mutated = true
	blocked := []string{
		"git checkout -- .",
		"git checkout .",
		"git reset --hard",
		"git reset --hard HEAD",
		"git restore .",
		"git clean -fd",
		"git stash",
	}
	for _, cmd := range blocked {
		_, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
			Args: map[string]any{"command": cmd}})
		if err == nil || !strings.Contains(err.Error(), "revert ALL changes") {
			t.Errorf("%q must be refused mid-task, got %v", cmd, err)
		}
	}
	allowed := []string{
		"git checkout -- main.go",   // single-file revert stays legal
		"git checkout featurebranch", // branch switch is not a tree revert
		"git status",
		"git diff",
	}
	for _, cmd := range allowed {
		if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
			Args: map[string]any{"command": cmd}}); err != nil {
			t.Errorf("%q must be allowed, got %v", cmd, err)
		}
	}
	// Before any mutation there is nothing of the agent's to protect.
	s2 := New(nil, nil, Options{Root: t.TempDir(), Out: io.Discard})
	if _, err := s2.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "git checkout -- ."}}); err != nil {
		t.Errorf("pre-mutation revert must be allowed (nothing to protect): %v", err)
	}
}
