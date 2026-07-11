package provider

import (
	"errors"
	"strings"
	"testing"
)

// A key must never survive into an error message.
func TestScrubRemovesKey(t *testing.T) {
	key := "sk-ant-SECRETSECRETSECRET"
	err := errors.New("HTTP 401 from https://api?auth=" + key + ": bad key " + key)
	got := scrub(err, key)
	if strings.Contains(got.Error(), key) {
		t.Fatalf("key leaked: %v", got)
	}
	if !strings.Contains(got.Error(), "[redacted]") {
		t.Fatalf("expected redaction marker: %v", got)
	}
	if scrub(nil, key) != nil {
		t.Fatal("scrub(nil) must be nil")
	}
}

// Paid providers must resolve keys through the injected resolver only.
func TestNewProviderUsesResolver(t *testing.T) {
	var asked []string
	getKey := func(vendor string) (string, error) {
		asked = append(asked, vendor)
		return "k", nil
	}
	if _, err := NewProvider("claude:claude-haiku-4-5-20251001", getKey); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProvider("openai:gpt-4o-mini", getKey); err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "anthropic,openai" {
		t.Fatalf("resolver calls = %v", asked)
	}
	// Local models must never touch credentials.
	asked = nil
	if _, err := NewProvider("ollama:qwen2.5-coder:14b", getKey); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProvider("qwen3-coder:30b", getKey); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Fatalf("local provider consulted credentials: %v", asked)
	}
}

func TestParseContentToolCall(t *testing.T) {
	tools := []ToolDef{{Name: "change_impact"}, {Name: "dead_code"}}
	c := parseContentToolCall(`{"name": "change_impact", "arguments": {"symbol": "A.b"}}`, tools)
	if c == nil || c.Name != "change_impact" || c.Args["symbol"] != "A.b" {
		t.Fatalf("fallback parse failed: %+v", c)
	}
	if parseContentToolCall(`{"name": "rm_rf", "arguments": {}}`, tools) != nil {
		t.Fatal("unknown tool must not parse")
	}
	if parseContentToolCall("plain prose", tools) != nil {
		t.Fatal("prose must not parse")
	}
}

// Multiple fenced tool calls in one content reply must all be recovered, in order.
func TestParseContentToolCallsMultiple(t *testing.T) {
	tools := []ToolDef{{Name: "rename_plan"}, {Name: "apply_rename_plan"}}
	content := "I will rename it.\n```json\n{\"name\": \"rename_plan\", \"arguments\": {\"symbol\": \"A.b\", \"newName\": \"c\"}}\n```\nthen apply:\n```json\n{\"name\": \"apply_rename_plan\", \"arguments\": {\"includeAmbiguous\": true}}\n```\n"
	calls := parseContentToolCalls(content, tools)
	if len(calls) != 2 || calls[0].Name != "rename_plan" || calls[1].Name != "apply_rename_plan" {
		t.Fatalf("calls = %+v", calls)
	}
	if v, _ := calls[1].Args["includeAmbiguous"].(bool); !v {
		t.Fatal("args lost")
	}
}

func TestEstimateCost(t *testing.T) {
	if c := EstimateCost("ollama:qwen3-coder:30b", 1e6, 1e6); c != 0 {
		t.Fatalf("local must be $0, got %f", c)
	}
	c := EstimateCost("claude:claude-haiku-4-5-20251001", 1_000_000, 1_000_000)
	if c != 6.0 { // $1 in + $5 out
		t.Fatalf("haiku cost = %f", c)
	}
	if EstimateCost("openai:gpt-4o-mini", 0, 1e6) != 0.60 {
		t.Fatal("4o-mini output price wrong")
	}
}
