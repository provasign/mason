package provider

import "strings"

// priceTable maps model-name substrings to USD per 1M input/output tokens.
// Estimates for cost VISIBILITY, not billing — providers' published list
// prices as of mid-2026; unknown models report 0 and are labeled estimates.
// Ordered: first match wins, so more-specific names come first.
var priceTable = []struct {
	match   string
	in, out float64
}{
	{"claude-fable", 25, 125},
	{"claude-opus", 15, 75},
	{"claude-sonnet", 3, 15},
	{"claude-haiku", 1, 5},
	{"gpt-4o-mini", 0.15, 0.60},
	{"gpt-4o", 2.50, 10},
	{"gpt-4.1-mini", 0.40, 1.60},
	{"gpt-4.1", 2, 8},
	{"o3", 2, 8},
	{"gemini-2.5-pro", 1.25, 10},
	{"gemini-2.5-flash", 0.30, 2.50},
	{"gemini-2.0-flash", 0.10, 0.40},
}

// EstimateCost returns the estimated USD cost of usage for a model spec.
// Local (ollama:) models are $0. Unknown paid models return 0 — callers
// should present token counts alongside, which are always exact.
func EstimateCost(spec string, in, out int) float64 {
	if strings.HasPrefix(spec, "ollama:") || !strings.Contains(spec, ":") {
		return 0
	}
	name := spec[strings.Index(spec, ":")+1:]
	for _, p := range priceTable {
		if strings.Contains(name, p.match) {
			return (float64(in)*p.in + float64(out)*p.out) / 1e6
		}
	}
	return 0
}
