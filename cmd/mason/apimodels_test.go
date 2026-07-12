package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/provasign/mason/internal/provider"
)

func TestResolveModelAlias(t *testing.T) {
	cases := map[string]string{
		"sonnet":   "claude:claude-sonnet-5",
		"SONNET":   "claude:claude-sonnet-5", // case-insensitive
		" haiku ":  "claude:claude-haiku-4-5-20251001",
		"opus":     "claude:claude-opus-4-8",
		"gpt":      "openai:gpt-4.1",
		"gpt-mini": "openai:gpt-4.1-mini",
		"fable":    "claude:claude-fable-5",
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

// renderModelList/renderModelListWith must NEVER touch the real keychain or
// network from a unit test — every test here supplies fakes explicitly,
// regardless of what credentials happen to be stored on the machine
// running the suite.
func noCred(string) bool { return false }

func TestVendorSpecPrefix(t *testing.T) {
	cases := map[string]string{"anthropic": "claude:", "openai": "openai:", "mistral": "mistral:"}
	for vendor, want := range cases {
		if got := vendorSpecPrefix(vendor); got != want {
			t.Errorf("vendorSpecPrefix(%q) = %q, want %q", vendor, got, want)
		}
	}
}

func TestRenderModelListSections(t *testing.T) {
	text, _, _, remote := renderModelListWith(noCred, nil)
	for _, want := range []string{"free local", "API —", "Claude Sonnet", "GPT-4.1",
		"/model sonnet"} {
		if !strings.Contains(text, want) {
			t.Errorf("model list missing %q:\n%s", want, text)
		}
	}
	if len(remote) != 0 {
		t.Fatal("no stored credentials means no live section")
	}
}

func TestRenderModelListLiveAugmentation(t *testing.T) {
	hasCred := func(vendor string) bool { return vendor == "anthropic" }
	fetch := func(vendor string) ([]provider.RemoteModel, error) {
		if vendor != "anthropic" {
			t.Fatalf("must not fetch for un-keyed vendor %q", vendor)
		}
		return []provider.RemoteModel{
			{ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5"},
			{ID: "claude-haiku-4-5-20251001", DisplayName: "Claude Haiku 4.5"},
		}, nil
	}
	text, _, _, remote := renderModelListWith(hasCred, fetch)
	if !strings.Contains(text, "every current anthropic model") {
		t.Fatalf("missing live section:\n%s", text)
	}
	if len(remote) != 2 || remote[0].Spec != "claude:claude-sonnet-5" {
		t.Fatalf("remote picks wrong: %+v", remote)
	}
}

func TestRenderModelListFetchErrorIsNonFatal(t *testing.T) {
	hasCred := func(string) bool { return true }
	fetch := func(string) ([]provider.RemoteModel, error) {
		return nil, errors.New("network down")
	}
	text, _, _, remote := renderModelListWith(hasCred, fetch)
	if len(remote) != 0 {
		t.Fatal("a fetch error must not produce picks")
	}
	if !strings.Contains(text, "free local") { // curated section still rendered
		t.Fatalf("curated section must survive a fetch error:\n%s", text)
	}
}
