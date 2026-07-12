package main

import (
	"strings"
	"testing"
)

func TestResolveModelAlias(t *testing.T) {
	cases := map[string]string{
		"sonnet":   "claude:claude-sonnet-5",
		"SONNET":   "claude:claude-sonnet-5", // case-insensitive
		" haiku ":  "claude:claude-haiku-4-5-20251001",
		"opus":     "claude:claude-opus-4-8",
		"gpt":      "openai:gpt-4.1",
		"gpt-mini": "openai:gpt-4.1-mini",
		// non-aliases pass through untouched
		"ollama:qwen3-coder:30b": "ollama:qwen3-coder:30b",
		"claude:claude-sonnet-5": "claude:claude-sonnet-5",
		"auto":                   "auto",
	}
	for in, want := range cases {
		if got := resolveModelAlias(in); got != want {
			t.Errorf("resolveModelAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAPICatalogIsAnthropicAndOpenAIOnly(t *testing.T) {
	if len(apiCatalog) == 0 {
		t.Fatal("catalog must not be empty")
	}
	for _, am := range apiCatalog {
		if am.Vendor != "anthropic" && am.Vendor != "openai" {
			t.Errorf("%s: vendor %q out of scope (anthropic/openai only for now)", am.Label, am.Vendor)
		}
		if am.Label == "" || am.Spec == "" || !strings.Contains(am.Spec, ":") {
			t.Errorf("malformed catalog entry: %+v", am)
		}
	}
	// Every alias resolves to a catalog spec, the local default, or auto —
	// aliases must never dangle.
	specs := map[string]bool{}
	for _, am := range apiCatalog {
		specs[am.Spec] = true
	}
	for alias, spec := range modelAliases {
		if !specs[spec] {
			t.Errorf("alias %q points at %q which is not in the catalog", alias, spec)
		}
	}
}

func TestRenderModelListSections(t *testing.T) {
	text, _, _ := renderModelList()
	for _, want := range []string{"free local", "API —", "Claude Sonnet", "GPT-4.1",
		"/model sonnet"} {
		if !strings.Contains(text, want) {
			t.Errorf("model list missing %q:\n%s", want, text)
		}
	}
}
