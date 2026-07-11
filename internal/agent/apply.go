package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// applyRenamePlan applies a rename plan's edits to the working tree,
// verifying each line still matches `before` first (drifted lines are
// skipped, never overwritten). Ambiguous edits are included only when the
// caller opted in — they may contain a same-named call on a different
// receiver, so the verify command is the safety net.
func applyRenamePlan(out io.Writer, root string, plan map[string]any, includeAmbiguous bool) (applied, skipped, ambiguousLeft int, err error) {
	edits := asSlice(plan["edits"])
	if includeAmbiguous {
		if amb := asSlice(plan["ambiguous"]); len(amb) > 0 {
			fmt.Fprintf(out, "\napply: including %d AMBIGUOUS edit(s) — verify is the safety net\n", len(amb))
			edits = append(edits, amb...)
		}
	}
	ambiguousLeft = 0
	if !includeAmbiguous {
		ambiguousLeft = len(asSlice(plan["ambiguous"]))
	}
	if len(edits) == 0 {
		fmt.Fprintln(out, "\napply: no confirmed edits to apply")
		return 0, 0, ambiguousLeft, nil
	}
	byFile := map[string][]map[string]any{}
	for _, e := range edits {
		em, _ := e.(map[string]any)
		if em == nil {
			continue
		}
		fp, _ := em["filePath"].(string)
		byFile[fp] = append(byFile[fp], em)
	}
	for fp, es := range byFile {
		path := fp
		if root != "" && !filepath.IsAbs(fp) {
			path = filepath.Join(root, fp)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return applied, skipped, ambiguousLeft, fmt.Errorf("apply: %s: %w", fp, rerr)
		}
		lines := strings.Split(string(data), "\n")
		for _, em := range es {
			lf, ok := em["line"].(float64)
			if !ok {
				skipped++
				continue
			}
			ln := int(lf) - 1
			before, _ := em["before"].(string)
			after, _ := em["after"].(string)
			if ln < 0 || ln >= len(lines) || strings.TrimRight(lines[ln], "\r") != before {
				fmt.Fprintf(out, "apply: SKIP %s:%d (line changed since plan)\n", fp, ln+1)
				skipped++
				continue
			}
			lines[ln] = after
			applied++
		}
		if werr := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); werr != nil {
			return applied, skipped, ambiguousLeft, fmt.Errorf("apply: %s: %w", fp, werr)
		}
	}
	fmt.Fprintf(out, "\napply: %d edit(s) applied, %d skipped", applied, skipped)
	if ambiguousLeft > 0 {
		fmt.Fprintf(out, "; %d AMBIGUOUS edits NOT applied", ambiguousLeft)
	}
	fmt.Fprintln(out)
	return applied, skipped, ambiguousLeft, nil
}
