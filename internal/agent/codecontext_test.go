package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

func TestCodeContextDeliversContent(t *testing.T) {
	invoked := map[string]any{}
	invoke := func(tool string, args map[string]any) (any, error) {
		if tool != "prism_query" {
			t.Fatalf("routed to %q", tool)
		}
		invoked = args
		return map[string]any{
			"budgetUsed": 900,
			"symbols": []any{
				map[string]any{"qualifiedName": "fetch.Retry", "filePath": "fetch.go",
					"category": "seed", "content": "func Retry() { … }"},
				map[string]any{"qualifiedName": "fetch.TestRetry", "filePath": "fetch_test.go",
					"category": "test", "content": "func TestRetry(t *testing.T) { … }"},
			},
		}, nil
	}
	s := New(nil, invoke, Options{Root: t.TempDir(), Out: io.Discard})
	out, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "code_context",
		Args: map[string]any{"task": "harden retry", "terms": []any{"Retry"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"== fetch.Retry [seed] — fetch.go", "func Retry()",
		"== fetch.TestRetry [test]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if terms, _ := invoked["terms"].([]string); len(terms) != 1 || terms[0] != "Retry" {
		t.Fatalf("terms not forwarded: %v", invoked["terms"])
	}
}

func TestCodingToolsOnlyStripsGraphTools(t *testing.T) {
	kept := map[string]bool{}
	for _, tl := range codingToolsOnly(toolDefs()) {
		kept[tl.Name] = true
	}
	for _, banned := range []string{"code_context", "change_impact", "apply_rename_plan", "dead_code"} {
		if kept[banned] {
			t.Errorf("%s must be stripped when the engine is off", banned)
		}
	}
	for _, want := range []string{"read_file", "grep", "bash", "web_fetch"} {
		if !kept[want] {
			t.Errorf("%s must survive engine-off", want)
		}
	}
}

func TestCodeContextEmptyAndErrors(t *testing.T) {
	invoke := func(tool string, args map[string]any) (any, error) {
		return map[string]any{"symbols": []any{}, "note": "no symbols matched terms"}, nil
	}
	s := New(nil, invoke, Options{Root: t.TempDir(), Out: io.Discard})
	out, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "code_context",
		Args: map[string]any{"task": "x", "terms": []any{"Zzz"}}})
	if err != nil || !strings.Contains(out, "no symbols matched") {
		t.Fatalf("empty result must surface the note: %q %v", out, err)
	}
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "code_context",
		Args: map[string]any{"task": "x", "terms": []any{}}}); err == nil {
		t.Fatal("no terms must error")
	}
	sNoEngine := New(nil, nil, Options{Root: t.TempDir(), Out: io.Discard})
	if _, err := sNoEngine.runCodingTool(context.Background(), provider.ToolCall{Name: "code_context",
		Args: map[string]any{"task": "x", "terms": []any{"a"}}}); err == nil {
		t.Fatal("engine-off must error cleanly")
	}
}
