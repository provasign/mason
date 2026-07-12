package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commandsRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".mason", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "fix-issue.md"),
		[]byte("Fix issue $ARGUMENTS: reproduce it, fix it, add a test.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "standup.md"),
		[]byte("Summarize the last day of git history.\n"), 0o644)
	return root
}

func TestExpandCommandSubstitutesArguments(t *testing.T) {
	root := commandsRoot(t)
	task, ok := expandCommand(root, "/fix-issue #42 login crash")
	if !ok || !strings.Contains(task, "Fix issue #42 login crash:") {
		t.Fatalf("expansion wrong: %q %v", task, ok)
	}
	// A template without $ARGUMENTS appends the args instead.
	task, ok = expandCommand(root, "/standup focus on mason")
	if !ok || !strings.HasSuffix(task, "focus on mason") {
		t.Fatalf("append fallback wrong: %q", task)
	}
	if _, ok := expandCommand(root, "/nope"); ok {
		t.Fatal("unknown command must not expand")
	}
	if _, ok := expandCommand(root, "/../secrets"); ok {
		t.Fatal("path traversal must not resolve a command")
	}
}

func TestListAndHelp(t *testing.T) {
	root := commandsRoot(t)
	names := listCommands(root)
	if len(names) != 2 || names[0] != "fix-issue" || names[1] != "standup" {
		t.Fatalf("listCommands: %v", names)
	}
	help := commandsHelp(root)
	if !strings.Contains(help, "/fix-issue") || !strings.Contains(help, "/standup") {
		t.Fatalf("help wrong: %q", help)
	}
	if commandsHelp(t.TempDir()) != "" {
		t.Fatal("no commands dir must yield empty help")
	}
}
