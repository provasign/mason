package agent

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Engine-verified branch review: everything here is deterministic — the
// code graph and the diff, no model in the loop. Output is a work list a
// reviewer (or CI) can trust: blast radius per changed symbol, coverage
// gaps, placeholder tests, stubs, and newly unreachable code.

// ReviewFinding is one item in the review report.
type ReviewFinding struct {
	Severity string // "warn" | "info"
	File     string
	Line     int
	Text     string
}

// ReviewReport is the full engine review of a diff.
type ReviewReport struct {
	Base         string
	ChangedFiles []string
	Impact       []string // blast-radius lines per changed symbol
	Findings     []ReviewFinding
}

// reviewBase picks the diff base: merge-base with origin/main, origin/master,
// main, master — else HEAD (working-tree-only review).
func reviewBase(root, requested string) string {
	git := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	if requested != "" {
		if _, err := git("rev-parse", "--verify", requested); err == nil {
			if mb, err := git("merge-base", requested, "HEAD"); err == nil {
				return mb
			}
			return requested
		}
	}
	for _, ref := range []string{"origin/main", "origin/master", "main", "master"} {
		if mb, err := git("merge-base", ref, "HEAD"); err == nil && mb != "" {
			return mb
		}
	}
	return "HEAD"
}

// wholeFile marks an untracked file: every symbol in it is "touched".
var wholeFile = []int{-1}

// diffLines returns the changed line numbers (new side) per file vs base,
// including uncommitted working-tree changes AND untracked files (git diff
// ignores those, but new files are the meat of a review).
func diffLines(root, base string) map[string][]int {
	res := map[string][]int{}
	if out, err := exec.Command("git", "-C", root, "ls-files", "-o", "--exclude-standard").Output(); err == nil {
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if f != "" {
				res[f] = wholeFile
			}
		}
	}
	out, err := exec.Command("git", "-C", root, "diff", "-U0", base).Output()
	if err != nil {
		return res
	}
	var cur string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			cur = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if !strings.HasPrefix(line, "@@") || cur == "" {
			continue
		}
		// @@ -a,b +start,count @@
		parts := strings.Fields(line)
		for _, p := range parts {
			if !strings.HasPrefix(p, "+") || p == "+++" {
				continue
			}
			nums := strings.SplitN(strings.TrimPrefix(p, "+"), ",", 2)
			start, _ := strconv.Atoi(nums[0])
			count := 1
			if len(nums) == 2 {
				count, _ = strconv.Atoi(nums[1])
			}
			for i := 0; i < max(count, 1); i++ {
				res[cur] = append(res[cur], start+i)
			}
			break
		}
	}
	return res
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// symbolsTouched maps changed lines to the symbols containing them: the
// nearest preceding symbol start (grove records start lines).
func symbolsTouched(syms []SymbolInfo, lines []int) []SymbolInfo {
	if len(syms) == 0 || len(lines) == 0 {
		return nil
	}
	sorted := append([]SymbolInfo(nil), syms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Line < sorted[j].Line })
	hit := map[string]SymbolInfo{}
	for _, ln := range lines {
		var cand *SymbolInfo
		for i := range sorted {
			if sorted[i].Line <= ln {
				cand = &sorted[i]
			} else {
				break
			}
		}
		if cand != nil {
			k := strings.ToLower(cand.Kind)
			if k == "function" || k == "method" {
				hit[cand.QualifiedName+cand.Name] = *cand
			}
		}
	}
	out := make([]SymbolInfo, 0, len(hit))
	for _, s := range hit {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// Review runs the engine review. Pure engine: needs the graph hooks, not a
// model.
func (s *Session) Review(base string) (*ReviewReport, error) {
	if s.invoke == nil || s.opts.FileSymbols == nil {
		return nil, fmt.Errorf("code graph unavailable — review needs the engine")
	}
	s.setStatus("review: diffing against base…")
	resolved := reviewBase(s.root, base)
	perFile := diffLines(s.root, resolved)
	if len(perFile) == 0 {
		return &ReviewReport{Base: resolved}, nil
	}
	// The graph must reflect the current tree. Continuing after a failed reindex
	// would make every downstream completeness claim unreliable.
	if _, err := s.invoke("prism_index", map[string]any{}); err != nil {
		return nil, fmt.Errorf("refresh code graph: %w", err)
	}

	rep := &ReviewReport{Base: resolved}
	var changed []string
	for f := range perFile {
		changed = append(changed, f)
	}
	sort.Strings(changed)
	rep.ChangedFiles = changed

	// 1) Textual gate findings (placeholder tests, stubs) on changed files.
	for _, q := range scanQuality(s.root, changed) {
		rep.Findings = append(rep.Findings, ReviewFinding{
			Severity: "warn", File: q.file, Line: q.line, Text: q.what})
	}

	// 2) Per touched symbol: blast radius + coverage.
	for _, f := range changed {
		if !isSourceFile(f) || testFileRe.MatchString(f) {
			continue
		}
		syms := s.opts.FileSymbols(f)
		var touched []SymbolInfo
		if len(perFile[f]) == 1 && perFile[f][0] == -1 {
			// untracked: every function/method is new
			for _, sym := range syms {
				k := strings.ToLower(sym.Kind)
				if k == "function" || k == "method" {
					touched = append(touched, sym)
				}
			}
		} else {
			touched = symbolsTouched(syms, perFile[f])
		}
		for _, sym := range touched {
			q := sym.QualifiedName
			if q == "" {
				q = sym.Name
			}
			s.setStatus("review: change_impact %s", q)
			if res, err := s.invoke("prism_change_impact", map[string]any{"query": q}); err == nil {
				if full, _ := res.(map[string]any); full != nil {
					callers := len(asSlice(full["callers"]))
					family := len(asSlice(full["family"]))
					completeness, _ := full["completeness"].(string)
					rep.Impact = append(rep.Impact, fmt.Sprintf(
						"%s (%s:%d) — %d caller(s), %d family member(s), %s",
						q, f, sym.Line, callers, family, completeness))
					if callers > 5 {
						rep.Findings = append(rep.Findings, ReviewFinding{Severity: "warn",
							File: f, Line: sym.Line,
							Text: fmt.Sprintf("%s has %d callers — verify each still holds after this change", q, callers)})
					}
				}
			} else {
				rep.Findings = append(rep.Findings, ReviewFinding{Severity: "warn",
					File: f, Line: sym.Line,
					Text: fmt.Sprintf("%s impact could not be verified: %v", q, err)})
			}
			// Coverage is authoritative only when the engine classifies the symbol.
			s.setStatus("review: coverage %s", q)
			res, err := s.invoke("prism_untested_surface", map[string]any{"query": q})
			if err != nil {
				rep.Findings = append(rep.Findings, ReviewFinding{Severity: "warn",
					File: f, Line: sym.Line,
					Text: fmt.Sprintf("%s coverage could not be verified: %v", q, err)})
				continue
			}
			full, _ := res.(map[string]any)
			if full == nil {
				rep.Findings = append(rep.Findings, ReviewFinding{Severity: "warn",
					File: f, Line: sym.Line,
					Text: q + " coverage could not be classified by the engine"})
				continue
			}
			if len(asSlice(full["covered"])) == 0 {
				text := sym.Name + " changed in this diff and no test covers it"
				if len(asSlice(full["untested"])) == 0 {
					text = q + " coverage could not be classified by the engine"
				}
				rep.Findings = append(rep.Findings, ReviewFinding{Severity: "warn",
					File: f, Line: sym.Line, Text: text})
			}
		}
	}

	// 3) Current unreachable code in the changed files. This is diagnostic only:
	// proving a regression requires a separate graph snapshot for the base tree.
	changedSet := map[string]bool{}
	for _, f := range changed {
		changedSet[f] = true
	}
	if res, err := s.invoke("prism_dead_code", map[string]any{}); err == nil {
		if full, _ := res.(map[string]any); full != nil {
			for _, d := range asSlice(full["dead"]) {
				dm, _ := d.(map[string]any)
				if dm == nil {
					continue
				}
				fp, _ := dm["filePath"].(string)
				if !changedSet[fp] {
					continue
				}
				name, _ := dm["qualifiedName"].(string)
				if name == "" {
					name, _ = dm["name"].(string)
				}
				rep.Findings = append(rep.Findings, ReviewFinding{Severity: "info",
					File: fp, Text: name + " is currently unreachable; no base-graph comparison was performed"})
			}
		}
	} else {
		rep.Findings = append(rep.Findings, ReviewFinding{Severity: "warn",
			Text: fmt.Sprintf("dead-code check could not be completed: %v", err)})
	}
	return rep, nil
}

// RenderReview prints the report for humans; returns the warn count so CI
// callers can gate on it.
func (rep *ReviewReport) Render(w func(string)) int {
	w(fmt.Sprintf("── engine review · base %.8s · %d changed file(s) ──", rep.Base, len(rep.ChangedFiles)))
	if len(rep.ChangedFiles) == 0 {
		w("no changes to review")
		return 0
	}
	if len(rep.Impact) > 0 {
		w("blast radius (engine-reported completeness per symbol):")
		for _, l := range rep.Impact {
			w("  " + l)
		}
	}
	warns := 0
	if len(rep.Findings) == 0 {
		w("no findings in the completed checks")
		return 0
	}
	w("findings:")
	for _, f := range rep.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		mark := "·"
		if f.Severity == "warn" {
			mark = "⚠"
			warns++
		}
		if loc != "" {
			w(fmt.Sprintf("  %s %s — %s", mark, loc, f.Text))
		} else {
			w(fmt.Sprintf("  %s %s", mark, f.Text))
		}
	}
	return warns
}
