package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/provasign/mason/internal/provider"
)

// asSlice normalizes a result group to []any: kit.Invoke returns typed Go
// slices ([]map[string]any, JSON-round-tripped structs), not []any — a bare
// type assertion would silently miss every group.
func asSlice(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = t[i]
		}
		return out
	default:
		b, err := json.Marshal(v)
		if err != nil || len(b) == 0 || b[0] != '[' {
			return nil
		}
		var out []any
		if json.Unmarshal(b, &out) != nil {
			return nil
		}
		return out
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// compactMeta is what the model sees from a graph operation: counts + flags,
// never payloads. Relay fidelity is structural: the site list cannot be
// dropped, re-filtered, or re-derived by the model because it never enters
// the model's context.
func compactMeta(op string, full map[string]any) string {
	m := map[string]any{}
	switch op {
	case "search_symbols":
		// The one exception: disambiguation NEEDS the names. Cap it small.
		var names []string
		if syms := asSlice(full["symbols"]); syms != nil {
			for _, s := range syms {
				if sm, ok := s.(map[string]any); ok {
					n, _ := sm["qualifiedName"].(string)
					if n == "" {
						n, _ = sm["name"].(string)
					}
					kind, _ := sm["kind"].(string)
					names = append(names, n+" ("+kind+")")
				}
				if len(names) >= 10 {
					break
				}
			}
		}
		m["matches"] = names
	default:
		for _, group := range []string{"declarations", "family", "callers",
			"declaringTypes", "supers", "edits", "ambiguous", "missing",
			"abstractMissing", "unverifiable", "untested", "covered", "dead",
			"exportedUnreferenced"} {
			if v := asSlice(full[group]); len(v) > 0 {
				m[group] = len(v)
			}
		}
		for _, scalar := range []string{"totalSites", "completeness",
			"implementedCount", "defaultProvided", "warning"} {
			if v, ok := full[scalar]; ok && v != nil {
				m[scalar] = v
			}
		}
		if v := asSlice(full["unresolved"]); len(v) > 0 {
			m["unresolved"] = v // short list; the agent must know these exist
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// render prints the deterministic result to the user — the harness, not
// the model, is the relay. maxItems caps each group (0 = unlimited): in
// the TUI the payload is a progress artifact more often than the
// deliverable, so it collapses to a head + "+N more" by default
// (/verbose shows everything).
func render(out io.Writer, call provider.ToolCall, full map[string]any, st style, maxItems int) {
	fmt.Fprintf(out, "\n── %s", call.Name)
	if s, ok := call.Args["symbol"].(string); ok {
		fmt.Fprintf(out, " %s", s)
	}
	if n, ok := call.Args["newName"].(string); ok {
		fmt.Fprintf(out, " → %s", n)
	}
	fmt.Fprintln(out, " ─────────────────────────────")

	site := func(v any) string {
		sm, _ := v.(map[string]any)
		if sm == nil {
			b, _ := json.Marshal(v)
			return string(b)
		}
		fp, _ := sm["filePath"].(string)
		name, _ := sm["qualifiedName"].(string)
		if name == "" {
			name, _ = sm["name"].(string)
		}
		line := ""
		if l, ok := sm["line"].(float64); ok {
			line = fmt.Sprintf(":%d", int(l))
		}
		return fmt.Sprintf("%s%s  %s", fp, line, name)
	}

	groups := []string{"declarations", "declaringTypes", "family", "supers",
		"callers", "missing", "abstractMissing", "unverifiable", "untested",
		"dead", "exportedUnreferenced", "symbols"}
	for _, g := range groups {
		items := asSlice(full[g])
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(out, "%s (%d):\n", g, len(items))
		lines := make([]string, 0, len(items))
		for _, it := range items {
			lines = append(lines, "  "+site(it))
		}
		sort.Strings(lines)
		shown := len(lines)
		if maxItems > 0 && shown > maxItems {
			shown = maxItems
		}
		for _, l := range lines[:shown] {
			fmt.Fprintln(out, l)
		}
		if shown < len(lines) {
			fmt.Fprintln(out, st.dim(fmt.Sprintf("  … +%d more (/verbose to show everything)", len(lines)-shown)))
		}
	}
	if edits := asSlice(full["edits"]); len(edits) > 0 {
		fmt.Fprintf(out, "edits (%d):\n", len(edits))
		shownE := 0
		for _, e := range edits {
			if maxItems > 0 && shownE >= maxItems {
				fmt.Fprintln(out, st.dim(fmt.Sprintf("  … +%d more edits (/verbose to show everything)", len(edits)-shownE)))
				break
			}
			shownE++
			em, _ := e.(map[string]any)
			if em == nil {
				continue
			}
			fmt.Fprintf(out, "  %s:%v\n    %s\n    %s\n",
				em["filePath"], em["line"],
				st.red("- "+strings.TrimSpace(fmt.Sprint(em["before"]))),
				st.green("+ "+strings.TrimSpace(fmt.Sprint(em["after"]))))
		}
	}
	for _, k := range []string{"completeness", "warning", "note", "unresolvedNote", "ambiguousNote"} {
		if v, ok := full[k].(string); ok && v != "" {
			fmt.Fprintf(out, "%s: %s\n", k, v)
		}
	}
	if u := asSlice(full["unresolved"]); len(u) > 0 {
		fmt.Fprintf(out, "unresolved (%d): %v\n", len(u), u)
	}
	if cav := asSlice(full["caveats"]); len(cav) > 0 {
		for _, c := range cav {
			fmt.Fprintf(out, "caveat: %v\n", c)
		}
	}
}

// intArg reads an integer tool argument that may arrive as float64 (JSON),
// int, or a numeric string, returning 0 when absent or malformed.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return 0
}

// renderTextSearch renders a prism_search scope=text result map as the plain
// grep-style text mason's model consumes: path:line: text hits (with context
// lines when requested), the truncation warning, the structural note (a
// searched symbol's implementations+callers), and the hit rollup (a truncated
// search's FULL hit set grouped by enclosing symbol). Returns "" for a
// result with no hits at all.
func renderTextSearch(m map[string]any) string {
	var b strings.Builder
	renderHits := func(mm map[string]any) bool {
		found := false
		if files := asSlice(mm["files"]); len(files) > 0 {
			for _, f := range files {
				fmt.Fprintf(&b, "%v\n", f)
			}
			return true
		}
		for _, g := range asSlice(mm["textHits"]) {
			gm, ok := g.(map[string]any)
			if !ok {
				continue
			}
			file, _ := gm["file"].(string)
			if cached, _ := gm["cached"].(bool); cached {
				lines := asSlice(gm["lines"])
				parts := make([]string, 0, len(lines))
				for _, l := range lines {
					parts = append(parts, fmt.Sprint(l))
				}
				fmt.Fprintf(&b, "%s: %s [cached — content already delivered this session]\n",
					file, strings.Join(parts, ","))
				found = true
				continue
			}
			for _, h := range asSlice(gm["hits"]) {
				hm, ok := h.(map[string]any)
				if !ok {
					continue
				}
				for _, l := range asSlice(hm["before"]) {
					fmt.Fprintf(&b, "%s-  %v\n", file, l)
				}
				fmt.Fprintf(&b, "%s:%v: %v\n", file, hm["line"], hm["text"])
				for _, l := range asSlice(hm["after"]) {
					fmt.Fprintf(&b, "%s-  %v\n", file, l)
				}
				found = true
			}
			if more, ok := gm["moreHits"]; ok {
				fmt.Fprintf(&b, "%s: +%v more matches\n", file, more)
			}
		}
		return found
	}
	got := false
	if groups := asSlice(m["results"]); len(groups) > 0 {
		for _, g := range groups {
			if gm, ok := g.(map[string]any); ok {
				if renderHits(gm) {
					got = true
				}
			}
		}
	} else {
		got = renderHits(m)
	}
	if w, _ := m["warning"].(string); w != "" {
		fmt.Fprintf(&b, "// %s\n", w)
	}
	if ru := asSlice(m["hitRollup"]); len(ru) > 0 {
		b.WriteString("// ALL matches by enclosing symbol (graph rollup of the full set):\n")
		for _, e := range ru {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if note, _ := em["note"].(string); note != "" {
				fmt.Fprintf(&b, "//   %s\n", note)
				continue
			}
			span, _ := em["span"].(map[string]any)
			fmt.Fprintf(&b, "//   %v  %v", em["symbol"], em["file"])
			if span != nil {
				fmt.Fprintf(&b, ":%v-%v", span["start"], span["end"])
			}
			fmt.Fprintf(&b, "  (%v hits)\n", em["hits"])
		}
		got = true
	}
	if n, _ := m["resolvedNote"].(string); n != "" {
		fmt.Fprintf(&b, "// %s\n", n)
		got = true
	}
	if !got {
		return ""
	}
	return b.String()
}
