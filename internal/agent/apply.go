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
func applyRenamePlan(out io.Writer, root string, plan map[string]any, includeAmbiguous bool) error {
	edits := asSlice(plan["edits"])
	if includeAmbiguous {
		if amb := asSlice(plan["ambiguous"]); len(amb) > 0 {
			fmt.Fprintf(out, "\napply: including %d AMBIGUOUS edit(s) — verify is the safety net\n", len(amb))
			edits = append(edits, amb...)
		}
	}
	if len(edits) == 0 {
		fmt.Fprintln(out, "\napply: no confirmed edits to apply")
		return nil
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
	applied, skipped := 0, 0
	for fp, es := range byFile {
		path := fp
		if root != "" && !filepath.IsAbs(fp) {
			path = filepath.Join(root, fp)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("apply: %s: %w", fp, err)
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
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			return fmt.Errorf("apply: %s: %w", fp, err)
		}
	}
	fmt.Fprintf(out, "\napply: %d edit(s) applied, %d skipped", applied, skipped)
	if amb := asSlice(plan["ambiguous"]); len(amb) > 0 && !includeAmbiguous {
		fmt.Fprintf(out, "; %d AMBIGUOUS edits NOT applied (say \"apply ambiguous too\" to include them with a verify)", len(amb))
	}
	fmt.Fprintln(out)
	return nil
}
