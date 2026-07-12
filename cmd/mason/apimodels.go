package main

import (
	"fmt"
	"strings"

	"github.com/provasign/mason/internal/creds"
	"github.com/provasign/mason/internal/localmodels"
	"github.com/provasign/mason/internal/provider"
	"github.com/provasign/mason/internal/tui"
)

// apiCatalog is the curated, recommended-first shortlist shown at the top
// of the picker — short and opinionated (Anthropic and OpenAI; local
// models come from localmodels). The FULL current lineup is never
// hand-maintained here: /models additionally queries each vendor's own
// live model-list endpoint for any vendor with a stored key, so the list
// can never go stale the way a static table would.
var apiCatalog = []tui.APIModel{
	{Label: "Claude Fable 5 — most capable", Spec: "claude:claude-fable-5", Vendor: "anthropic"},
	{Label: "Claude Sonnet 5 — top coding quality", Spec: "claude:claude-sonnet-5", Vendor: "anthropic"},
	{Label: "Claude Haiku 4.5 — fast + cheap", Spec: "claude:claude-haiku-4-5-20251001", Vendor: "anthropic"},
	{Label: "Claude Opus 4.8 — hardest reasoning", Spec: "claude:claude-opus-4-8", Vendor: "anthropic"},
	{Label: "GPT-4.1 — OpenAI coding flagship", Spec: "openai:gpt-4.1", Vendor: "openai"},
	{Label: "GPT-4.1 mini — fast + cheap", Spec: "openai:gpt-4.1-mini", Vendor: "openai"},
}

// modelAliases map the names people actually type to full specs. Applied
// to --model and /model arguments; unknown strings pass through untouched
// so full specs and ollama tags keep working.
var modelAliases = map[string]string{
	"fable":    "claude:claude-fable-5",
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

// vendorSpecPrefix maps a credential vendor to its provider-spec prefix.
func vendorSpecPrefix(vendor string) string {
	switch vendor {
	case "anthropic":
		return "claude:"
	case "openai":
		return "openai:"
	default:
		return vendor + ":"
	}
}

// fetchRemoteModels queries vendor's live model list using the vendor's
// stored (or env) credential — the answer to "how do we keep the list up
// to date": ask the vendor, don't hand-maintain it. Errors are swallowed
// to "" (best-effort, non-fatal — the curated shortlist still works).
func fetchRemoteModels(vendor string) ([]provider.RemoteModel, error) {
	key, err := creds.Get(vendor, false)
	if err != nil {
		return nil, err
	}
	return provider.ListModels(vendor, key)
}

// remotePick is one live-fetched model, ready to switch to (cred already
// confirmed present by construction).
type remotePick struct {
	Vendor, Spec, Label string
}

const maxRemoteShown = 15 // live lists (esp. OpenAI's) can run long; the rest reach via a typed spec

// renderModelList builds the unified picker text for the line-mode REPL and
// returns the pick tables that map numbers back to choices.
func renderModelList() (text string, installed []string, download []localmodels.Model, remote []remotePick) {
	return renderModelListWith(creds.Has, fetchRemoteModels)
}

// renderModelListWith takes hasCred/fetch as parameters so tests can supply
// fakes — real keychain state and network calls must never leak into a
// unit test just because renderModelList() is exercised. The live section
// is fetched synchronously (bounded to 10s per vendor by fetchRemoteModels)
// — REPL commands are synchronous elsewhere too (e.g. an ollama pull blocks).
func renderModelListWith(hasCred func(vendor string) bool, fetch func(vendor string) ([]provider.RemoteModel, error)) (text string, installed []string, download []localmodels.Model, remote []remotePick) {
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
	b.WriteString("API — recommended, /model N switches (mason guides you through the key on first use):\n")
	for _, am := range apiCatalog {
		n++
		status := "needs API key — mason will walk you through it"
		if hasCred(am.Vendor) {
			status = "✓ key in keychain"
		}
		fmt.Fprintf(&b, "  %d. %-36s %s\n", n, am.Label, status)
	}
	if fetch != nil {
		seen := map[string]bool{}
		for _, am := range apiCatalog {
			if seen[am.Vendor] || !hasCred(am.Vendor) {
				continue
			}
			seen[am.Vendor] = true
			models, err := fetch(am.Vendor)
			if err != nil || len(models) == 0 {
				continue
			}
			if len(models) > maxRemoteShown {
				models = models[:maxRemoteShown]
			}
			fmt.Fprintf(&b, "API — every current %s model (live from the API):\n", am.Vendor)
			prefix := vendorSpecPrefix(am.Vendor)
			for _, rm := range models {
				n++
				remote = append(remote, remotePick{Vendor: am.Vendor, Spec: prefix + rm.ID, Label: rm.Label()})
				fmt.Fprintf(&b, "  %d. %s\n", n, rm.Label())
			}
		}
	}
	b.WriteString("also: /model sonnet · haiku · opus · fable · gpt · gpt-mini — or any full spec")
	return b.String(), st.Installed, download, remote
}
