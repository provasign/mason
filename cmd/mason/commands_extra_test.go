package main

import (
	"slices"
	"strings"
	"testing"
)

func slicesContains(s []string, v string) bool { return slices.Contains(s, v) }

func TestCustomCommandInfos(t *testing.T) {
	root := commandsRoot(t) // defined in commands_test.go: fix-issue, standup
	infos := customCommandInfos(root)
	if len(infos) != 2 {
		t.Fatalf("want 2 custom commands, got %+v", infos)
	}
	byName := map[string]string{}
	for _, c := range infos {
		byName[c.Name] = c.Desc
	}
	if !strings.HasPrefix(byName["fix-issue"], "Fix issue") {
		t.Fatalf("desc must come from the template's first line: %q", byName["fix-issue"])
	}
	if customCommandInfos(t.TempDir()) != nil {
		t.Fatal("no commands dir must yield nil")
	}
}

func TestReplCommandNamesMatchTUITable(t *testing.T) {
	names := replCommandNames()
	if len(names) == 0 {
		t.Fatal("must not be empty")
	}
	// "models" is deliberately absent: merged into /model (kept only as a
	// hidden legacy alias, not advertised or completed).
	want := map[string]bool{"model": true, "help": true, "mouse": true, "exit": true}
	if slicesContains(names, "models") {
		t.Error("'models' must not be advertised — it merged into /model")
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("replCommandNames missing %q", n)
		}
	}
}
