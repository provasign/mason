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

func TestCodeContextPassesThroughSourceDelivery(t *testing.T) {
	// prism v0.25 source delivery: the response is one pre-rendered content
	// string (line-numbered windows + anchor summary), not a symbols list.
	// It must reach the model verbatim, not fall into the "(no context)" path.
	rendered := "**Anchors — callers and covering tests (verify before editing)**\n\n" +
		"- `Retry` (fetch.go:10) — 3 callers in `main.go`; tests: `fetch_test.go`\n\n" +
		"**`fetch.go`**\n\n```go\n10\tfunc Retry() {\n11\t\t…\n12\t}\n```\n"
	invoke := func(tool string, args map[string]any) (any, error) {
		return map[string]any{
			"content":         rendered,
			"delivery":        "source",
			"files":           []any{"fetch.go"},
			"deliveredTokens": 60,
		}, nil
	}
	s := New(nil, invoke, Options{Root: t.TempDir(), Out: io.Discard})
	out, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "code_context",
		Args: map[string]any{"task": "fix the retry bug", "terms": []any{"Retry"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Anchors", "10\tfunc Retry() {", "3 callers"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(no context") {
		t.Errorf("source delivery fell into the no-context path:\n%s", out)
	}
}

func TestCodeContextForcesSourceDeliveryOnMutationTask(t *testing.T) {
	// The model phrases context calls as "analyze/understand X" even mid-fix,
	// which phase-detects as explore. The USER task decides the path: a
	// mutation task routes through the unified `prism` prepare call (source
	// delivery + change obligations); a read-only task stays on prism_query
	// with delivery left to prism's phase detection.
	var gotTool string
	var got map[string]any
	invoke := func(tool string, args map[string]any) (any, error) {
		gotTool, got = tool, args
		if tool == "prism" {
			return map[string]any{
				"read": map[string]any{"content": "**Source**\n1\tx\n", "delivery": "source"},
				"obligations": []any{map[string]any{
					"qualifiedName": "fetcher.Retry", "completeness": "closed",
					"siteCount": float64(3),
					"sites": []any{map[string]any{"symbol": "app.Use", "file": "app/use.go", "line": float64(9)}},
				}},
			}, nil
		}
		return map[string]any{"content": "**Source**\n1\tx\n", "delivery": "source"}, nil
	}
	s := New(nil, invoke, Options{Root: t.TempDir(), Out: io.Discard})

	s.curTask = "fix the retry bug in the fetcher"
	out, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "code_context",
		Args: map[string]any{"task": "analyze the retry logic", "terms": []any{"Retry"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotTool != "prism" {
		t.Errorf("mutation task should route to the unified prism prepare call, got %q", gotTool)
	}
	if !strings.Contains(out, "CHANGE OBLIGATIONS") || !strings.Contains(out, "fetcher.Retry") {
		t.Errorf("obligations missing from rendered context:\n%s", out)
	}

	s.curTask = "explain how the retry logic works"
	gotTool, got = "", nil
	if _, err := s.runCodingTool(context.Background(), provider.ToolCall{Name: "code_context",
		Args: map[string]any{"task": "analyze the retry logic", "terms": []any{"Retry"}}}); err != nil {
		t.Fatal(err)
	}
	if gotTool != "prism_query" {
		t.Errorf("read-only task should stay on prism_query, got %q", gotTool)
	}
	if _, present := got["delivery"]; present {
		t.Errorf("read-only task must not force delivery, got %v", got["delivery"])
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
