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

// render prints the full deterministic result to the user — the harness,
// not the model, is the relay.
func render(out io.Writer, call provider.ToolCall, full map[string]any, st style) {
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
		for _, l := range lines {
			fmt.Fprintln(out, l)
		}
	}
	if edits := asSlice(full["edits"]); len(edits) > 0 {
		fmt.Fprintf(out, "edits (%d):\n", len(edits))
		for _, e := range edits {
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
