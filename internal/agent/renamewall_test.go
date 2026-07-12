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

// renamePlanResult fakes a prism rename_plan payload with n ambiguous edits.
func renamePlanResult(nAmb int) map[string]any {
	amb := make([]any, nAmb)
	for i := range amb {
		amb[i] = map[string]any{"filePath": "x_test.go", "line": float64(i + 1),
			"before": "old", "after": "new"}
	}
	return map[string]any{
		"edits":     []any{map[string]any{"filePath": "a.go", "line": float64(1), "before": "old", "after": "new"}},
		"ambiguous": amb,
	}
}

// The rename wall: after rename_plan the ONLY tool on offer is
// apply_rename_plan (forced); while ambiguous edits remain the set is
// {apply_rename_plan, bash}; a second apply reopens the full set.
func TestRenameWallNarrowsAndReopens(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		// turn 0 (graph wall): produce the plan
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "1", Name: "rename_plan",
			Args: map[string]any{"symbol": "ResponseWriter.Status", "newName": "StatusCode"}}}},
		// turn 1 (rename wall): the model IGNORES the restriction and tries
		// to re-plan — local providers don't honor tool lists strictly, so
		// dispatch itself must refuse (measured failure mode).
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "rogue", Name: "rename_plan",
			Args: map[string]any{"symbol": "responseWriter.Status", "newName": "StatusCode"}}}},
		// turn 2 (still walled): apply without ambiguous
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "2", Name: "apply_rename_plan",
			Args: map[string]any{}}}},
		// turn 3 (ambiguous wall): apply WITH ambiguous
		{Role: "assistant", Calls: []provider.ToolCall{{ID: "3", Name: "apply_rename_plan",
			Args: map[string]any{"includeAmbiguous": true}}}},
		// turn 4 (full set again): finish
		{Role: "assistant", Content: "renamed and verified"},
	}}
	planCalls := 0
	invoke := func(tool string, args map[string]any) (any, error) {
		switch tool {
		case "prism_rename_plan":
			planCalls++
			return renamePlanResult(3), nil
		case "prism_index", "prism_untested_surface", "prism_dead_code":
			return map[string]any{}, nil // quality-gate machinery, not under test
		default:
			t.Fatalf("unexpected engine op %q", tool)
			return nil, nil
		}
	}
	root := t.TempDir()
	// applyRenamePlan reads real files — give the plan real targets.
	for _, f := range []string{"a.go", "x_test.go"} {
		if err := os.WriteFile(filepath.Join(root, f),
			[]byte("old\nold\nold\nold\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := New(fp, invoke, Options{Root: root, Out: io.Discard})
	if _, err := s.Ask(context.Background(), "rename ResponseWriter.Status to StatusCode everywhere"); err != nil {
		t.Fatal(err)
	}
	if len(fp.turns) < 5 {
		t.Fatalf("want 5 turns, got %d", len(fp.turns))
	}
	names := func(i int) map[string]bool {
		m := map[string]bool{}
		for _, n := range fp.turns[i].toolNames {
			m[n] = true
		}
		return m
	}
	// turn 0: graph wall (forced), rename_plan available
	if !fp.turns[0].forced || !names(0)["rename_plan"] {
		t.Fatalf("turn 0 must be the forced graph wall: %+v", fp.turns[0])
	}
	// turn 1: ONLY apply_rename_plan, forced
	if got := fp.turns[1]; !got.forced || len(got.toolNames) != 1 || got.toolNames[0] != "apply_rename_plan" {
		t.Fatalf("turn 1 must offer only apply_rename_plan (forced): %+v", got)
	}
	// The rogue rename_plan on turn 1 must have been REFUSED at dispatch:
	// the engine saw exactly one plan call, and the model got a redirect.
	if planCalls != 1 {
		t.Fatalf("rogue mid-wall rename_plan must not reach the engine (plan calls = %d)", planCalls)
	}
	rejected := false
	for _, seen := range fp.seen {
		if strings.Contains(seen, "not available while a rename plan is pending") {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("the model must receive the redirect error for the rogue call")
	}
	// turn 2: still walled to apply_rename_plan only
	if got := fp.turns[2]; len(got.toolNames) != 1 || got.toolNames[0] != "apply_rename_plan" {
		t.Fatalf("turn 2 must stay walled: %+v", got)
	}
	// turn 3: ambiguous pending → {apply_rename_plan, bash}
	n3 := names(3)
	if len(n3) != 2 || !n3["apply_rename_plan"] || !n3["bash"] {
		t.Fatalf("turn 3 must offer exactly {apply_rename_plan, bash}: %v", fp.turns[3].toolNames)
	}
	// turn 4: full set restored (read_file/edit_file as proxies)
	if !names(4)["read_file"] || !names(4)["edit_file"] {
		t.Fatalf("turn 4 must restore the full toolset: %v", fp.turns[4].toolNames)
	}
}

// A green build releases the ambiguous wall (multi-part tasks continue with
// the full toolset even when the model chose not to apply ambiguous edits).
func TestRenameWallReleasedByGreenBuild(t *testing.T) {
	s := New(nil, nil, Options{Root: t.TempDir(), Out: io.Discard})
	s.renameAmbiguousLeft = 5
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "go build ./..."}}); err != nil {
		t.Fatal(err)
	}
	// go build in an empty temp dir fails — the wall must NOT release.
	if s.renameAmbiguousLeft != 5 {
		t.Fatal("a failing build must not release the rename wall")
	}
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "true"}}); err != nil {
		t.Fatal(err)
	}
	if s.renameAmbiguousLeft != 5 {
		t.Fatal("a non-build command must not release the wall")
	}
	// A green verify command does.
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "bash",
		Args: map[string]any{"command": "go version && echo go build ok"}}); err != nil {
		t.Fatal(err)
	}
	if s.renameAmbiguousLeft != 0 {
		t.Fatal("a green build command must release the rename wall")
	}
}

// The wall state is per-task: a stale plan from a previous Ask must not
// restrict the next one.
func TestRenameWallResetsPerTask(t *testing.T) {
	fp := &fakeProvider{replies: []provider.Msg{
		{Role: "assistant", Content: "hello"},
	}}
	s := New(fp, nil, Options{Root: t.TempDir(), Out: io.Discard})
	s.renamePlanPending = true
	s.renameAmbiguousLeft = 7
	if _, err := s.Ask(context.Background(), "what is this repo"); err != nil {
		t.Fatal(err)
	}
	if got := fp.turns[0]; len(got.toolNames) < 5 {
		t.Fatalf("new task must start with the full toolset, got %v", got.toolNames)
	}
}
