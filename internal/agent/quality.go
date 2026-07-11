package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// The quality gate: placeholder tests and unimplemented stubs are
// deterministically detectable, so the harness detects them and forces the
// fix — the model does not get to declare victory over scaffolding.
// (Measured: a 30B shipped assertTrue(True) tests and never installed or
// ran anything, then reported success.)

var testFileRe = regexp.MustCompile(`(^|/)(test_[^/]+\.py|[^/]+_test\.(go|py|rb|ts|js)|[^/]+\.(test|spec)\.(ts|js|tsx|jsx)|Test[^/]+\.java)$`)

// testRunRe recognizes bash commands that actually execute tests.
var testRunRe = regexp.MustCompile(`\b(pytest|go test|npm (run )?test|yarn test|jest|vitest|cargo test|mvn test|gradle test|unittest|rspec|phpunit|ctest)\b`)

var vacuousTestRes = []*regexp.Regexp{
	regexp.MustCompile(`assertTrue\(\s*True\s*\)`),
	regexp.MustCompile(`assertEqual\(\s*(True|1)\s*,\s*(True|1)\s*\)`),
	regexp.MustCompile(`(?m)^\s*assert\s+True\s*(#.*)?$`),
	regexp.MustCompile(`expect\(\s*true\s*\)\s*\.\s*(toBe|toEqual)\(\s*true\s*\)`),
	regexp.MustCompile(`\bt\.Skip\(`),
	regexp.MustCompile(`@(pytest\.mark\.skip|unittest\.skip|Disabled)\b`),
	// a test whose entire body is pass / ... / return
	regexp.MustCompile(`(?m)^\s*def test_\w+\([^)]*\):\s*\n\s+(pass|\.\.\.)\s*$`),
}

var stubRes = []*regexp.Regexp{
	regexp.MustCompile(`\bNotImplementedError\b`),
	regexp.MustCompile(`(?i)\bTODO:?\s*implement`),
	regexp.MustCompile(`(?i)panic\(\s*"(TODO|not implemented|unimplemented)`),
	regexp.MustCompile(`(?i)\bunimplemented!?\(`),
	regexp.MustCompile(`(?i)raise\s+NotImplemented\b`),
	// a non-test python function whose entire body is pass / ...
	regexp.MustCompile(`(?m)^\s*def (?:[a-su-z]\w*|t(?:[a-df-z]\w*)?|te(?:[a-rt-z]\w*)?)\([^)]*\):\s*\n\s+(pass|\.\.\.)\s*$`),
}

// changedFilesSince diffs the porcelain status against the snapshot taken at
// Ask start: anything whose status line changed (or is new) was touched by
// this task. Falls back to nothing outside git.
func changedFilesSince(root string, startStatus map[string]string) []string {
	cur := porcelainStatus(root)
	var out []string
	for path, line := range cur {
		if startStatus[path] != line {
			out = append(out, path)
		}
	}
	return out
}

// porcelainStatus maps repo-relative path → a state signature. -uall lists
// untracked files INDIVIDUALLY, and untracked entries get a size+mtime
// signature appended: git shows no content state for them, so an edit to an
// already-untracked file would otherwise be invisible — which blinded the
// honesty guard and the quality gate in fresh projects (measured).
func porcelainStatus(root string) map[string]string {
	out := map[string]string{}
	b, err := exec.Command("git", "-C", root, "status", "--porcelain", "-uall").Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		// renames: "old -> new"
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		sig := line
		if strings.HasPrefix(line, "??") || strings.HasPrefix(line, "A ") || strings.HasPrefix(line, "AM") {
			if fi, err := os.Stat(filepath.Join(root, path)); err == nil {
				sig += fmt.Sprintf("|%d|%d", fi.Size(), fi.ModTime().UnixNano())
			}
		}
		out[path] = sig
	}
	return out
}

// qualityFinding is one concrete defect the gate will force the model to fix.
type qualityFinding struct {
	file string
	line int
	what string
}

func (f qualityFinding) String() string {
	return fmt.Sprintf("%s:%d — %s", f.file, f.line, f.what)
}

// scanQuality inspects changed files for placeholder tests and stub code.
func scanQuality(root string, changed []string) []qualityFinding {
	var out []qualityFinding
	for _, rel := range changed {
		abs := filepath.Join(root, rel)
		data, err := os.ReadFile(abs)
		if err != nil || len(data) > 1<<20 || isBinary(data) {
			continue
		}
		content := string(data)
		isTest := testFileRe.MatchString(rel)
		if isTest {
			for _, re := range vacuousTestRes {
				for _, loc := range re.FindAllStringIndex(content, 5) {
					out = append(out, qualityFinding{
						file: rel, line: lineOf(content, loc[0]),
						what: "placeholder test (" + firstLine(content[loc[0]:loc[1]]) + ") — asserts nothing about real behavior",
					})
				}
			}
		}
		for _, re := range stubRes {
			for _, loc := range re.FindAllStringIndex(content, 5) {
				out = append(out, qualityFinding{
					file: rel, line: lineOf(content, loc[0]),
					what: "unimplemented stub (" + firstLine(content[loc[0]:loc[1]]) + ")",
				})
			}
		}
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// touchedTests reports whether any changed file is a test file.
func touchedTests(changed []string) bool {
	for _, f := range changed {
		if testFileRe.MatchString(f) {
			return true
		}
	}
	return false
}

func lineOf(s string, off int) int {
	return 1 + strings.Count(s[:off], "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 60)
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 512 {
		n = 512
	}
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}
