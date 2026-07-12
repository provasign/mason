package main

import (
	"fmt"
	"strings"

	"github.com/provasign/mason/internal/creds"
	"github.com/provasign/mason/internal/localmodels"
	"github.com/provasign/mason/internal/tui"
)

// apiCatalog is the curated paid-model list shown in the unified picker —
// short, opinionated, and limited to the vendors mason guides users
// through (Anthropic and OpenAI; local models come from localmodels).
var apiCatalog = []tui.APIModel{
	{Label: "Claude Sonnet — top coding quality", Spec: "claude:claude-sonnet-5", Vendor: "anthropic"},
	{Label: "Claude Haiku — fast + cheap", Spec: "claude:claude-haiku-4-5-20251001", Vendor: "anthropic"},
	{Label: "Claude Opus — hardest reasoning", Spec: "claude:claude-opus-4-8", Vendor: "anthropic"},
	{Label: "GPT-4.1 — OpenAI coding flagship", Spec: "openai:gpt-4.1", Vendor: "openai"},
	{Label: "GPT-4.1 mini — fast + cheap", Spec: "openai:gpt-4.1-mini", Vendor: "openai"},
}

// modelAliases map the names people actually type to full specs. Applied
// to --model and /model arguments; unknown strings pass through untouched
// so full specs and ollama tags keep working.
var modelAliases = map[string]string{
	"sonnet":   "claude:claude-sonnet-5",
	"haiku":    "claude:claude-haiku-4-5-20251001",
	"opus":     "claude:claude-opus-4-8",
	"claude":   "claude:claude-sonnet-5",
	"gpt":      "openai:gpt-4.1",
	"gpt-mini": "openai:gpt-4.1-mini",
	"mini":     "openai:gpt-4.1-mini",
}

// resolveModelAlias expands a friendly alias ("sonnet") to its full spec.
func resolveModelAlias(spec string) string {
	if full, ok := modelAliases[strings.ToLower(strings.TrimSpace(spec))]; ok {
		return full
	}
	return spec
}

// renderModelList builds the unified picker text for the line-mode REPL and
// returns the pick tables that map numbers back to choices.
func renderModelList() (text string, installed []string, download []localmodels.Model) {
	st := localmodels.Detect()
	ram := localmodels.SystemRAMGB()
	installedSet := st.InstalledSet()
	var b strings.Builder
	b.WriteString("free local — installed, /model N switches:\n")
	for i, t := range st.Installed {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, t)
	}
	b.WriteString("free local — /model N downloads then switches:\n")
	n := len(st.Installed)
	for _, c := range localmodels.Catalog {
		if !installedSet[c.Tag] && c.Fits(ram) {
			n++
			download = append(download, c)
			fmt.Fprintf(&b, "  %d. %-22s %.1f GB · needs %d GB — %s\n", n, c.Tag, c.DownloadGB, c.MinRAMGB, c.Note)
		}
	}
	b.WriteString("API — /model N switches (mason guides you through the key on first use):\n")
	for _, am := range apiCatalog {
		n++
		status := "needs API key — mason will walk you through it"
		if creds.Has(am.Vendor) {
			status = "✓ key in keychain"
		}
		fmt.Fprintf(&b, "  %d. %-36s %s\n", n, am.Label, status)
	}
	b.WriteString("also: /model sonnet · haiku · opus · gpt · gpt-mini — or any full spec")
	return b.String(), st.Installed, download
}
