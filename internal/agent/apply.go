package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// applyRenamePlan applies a rename plan's edits to the working tree,
// verifying each line still matches `before` first (drifted lines are
// skipped, never overwritten). Ambiguous edits are included only when the
// caller opted in — they may contain a same-named call on a different
// receiver, so the verify command is the safety net.
//
// All-or-nothing: every file is read and rewritten IN MEMORY first, and
// nothing touches disk until every edit has been validated. A rename is the
// one operation where a partial apply is worse than no apply — half a rename
// is a tree that does not build, and the model's next move is to hand-patch
// the remainder, which is exactly the spiral the rename wall exists to
// prevent. If a write still fails midway (disk full, permissions), the files
// already written are restored from their original bytes before the error
// propagates.
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
	// Map order is random; the SKIP lines the user reads (and the order a
	// partial failure would report) must not vary run to run.
	files := make([]string, 0, len(byFile))
	for fp := range byFile {
		files = append(files, fp)
	}
	sort.Strings(files)

	type pending struct {
		path string
		orig []byte
		next []byte
		mode os.FileMode
	}
	var writes []pending

	for _, fp := range files {
		path, perr := resolveEditPath(root, fp)
		if perr != nil {
			return 0, 0, ambiguousLeft, fmt.Errorf("apply: %w", perr)
		}
		info, serr := os.Stat(path)
		if serr != nil {
			return 0, 0, ambiguousLeft, fmt.Errorf("apply: %s: %w", fp, serr)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return 0, 0, ambiguousLeft, fmt.Errorf("apply: %s: %w", fp, rerr)
		}
		lines := strings.Split(string(data), "\n")
		touched := false
		for _, em := range byFile[fp] {
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
			touched = true
		}
		if !touched {
			continue
		}
		writes = append(writes, pending{
			path: path, orig: data,
			next: []byte(strings.Join(lines, "\n")),
			mode: info.Mode().Perm(),
		})
	}

	for i, w := range writes {
		if werr := os.WriteFile(w.path, w.next, w.mode); werr != nil {
			// Roll back everything already written so the tree is exactly
			// as the user left it. A rollback that itself fails is reported
			// — silently leaving a half-renamed tree is not an option.
			var rollbackErr error
			for _, done := range writes[:i] {
				if rerr := os.WriteFile(done.path, done.orig, done.mode); rerr != nil && rollbackErr == nil {
					rollbackErr = rerr
				}
			}
			if rollbackErr != nil {
				return 0, 0, ambiguousLeft, fmt.Errorf(
					"apply: %s: %w (ROLLBACK ALSO FAILED: %v — the working tree is partially renamed, inspect it before continuing)",
					w.path, werr, rollbackErr)
			}
			return 0, 0, ambiguousLeft, fmt.Errorf("apply: %s: %w (no files changed — rolled back)", w.path, werr)
		}
	}

	fmt.Fprintf(out, "\napply: %d edit(s) applied, %d skipped", applied, skipped)
	if ambiguousLeft > 0 {
		fmt.Fprintf(out, "; %d AMBIGUOUS edits NOT applied", ambiguousLeft)
	}
	fmt.Fprintln(out)
	return applied, skipped, ambiguousLeft, nil
}

// resolveEditPath turns a plan's filePath into an absolute path and refuses
// anything that lands outside root. The plan is engine output, but it is
// engine output about a repo — an absolute path or a "../.." prefix reaching
// out of the project is never a legitimate rename site, and apply runs behind
// a single approval that the user granted for THIS project.
func resolveEditPath(root, fp string) (string, error) {
	if fp == "" {
		return "", fmt.Errorf("edit with empty filePath")
	}
	if root == "" {
		return fp, nil
	}
	path := fp
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(filepath.Clean(root), path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: outside the project root", fp)
	}
	return path, nil
}
