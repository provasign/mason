package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rename must be all-or-nothing. When any file in the plan cannot be read,
// NOTHING is written — half a rename is a tree that does not build, and the
// model's next move is to hand-patch the remainder, which is the exact spiral
// the rename wall exists to prevent.
func TestApplyRenamePlanIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "a.go")
	orig := "package p\n\nfunc GetById() {}\n"
	if err := os.WriteFile(good, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{"edits": []any{
		map[string]any{"filePath": "a.go", "line": float64(3),
			"before": "func GetById() {}", "after": "func GetDataKeyById() {}"},
		// b.go does not exist: the whole apply must abort.
		map[string]any{"filePath": "b.go", "line": float64(1),
			"before": "x", "after": "y"},
	}}
	applied, _, _, err := applyRenamePlan(io.Discard, dir, plan, false)
	if err == nil {
		t.Fatal("expected an error when a plan file is missing")
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0 on abort", applied)
	}
	got, _ := os.ReadFile(good)
	if string(got) != orig {
		t.Errorf("a.go was modified despite the abort:\n%s", got)
	}
}

// Edits must stay inside the project root. An absolute path or a "../" prefix
// is never a legitimate rename site, and apply runs behind one approval the
// user granted for THIS project.
func TestApplyRenamePlanRefusesEscapingPaths(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.go")
	orig := "package p // untouched\n"
	if err := os.WriteFile(outside, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{outside, "../secret.go", "sub/../../secret.go"} {
		plan := map[string]any{"edits": []any{
			map[string]any{"filePath": fp, "line": float64(1),
				"before": "package p // untouched", "after": "package p // OWNED"},
		}}
		_, _, _, err := applyRenamePlan(io.Discard, root, plan, false)
		if err == nil || !strings.Contains(err.Error(), "outside the project root") {
			t.Errorf("filePath %q: err = %v, want an out-of-root refusal", fp, err)
		}
	}
	got, _ := os.ReadFile(outside)
	if string(got) != orig {
		t.Errorf("file outside the root was written: %s", got)
	}
}

// Applying must not silently re-permission a file (a 0755 script rewritten
// 0644 stops being executable, and nothing in a rename asked for that).
func TestApplyRenamePlanPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte("GetById\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{"edits": []any{
		map[string]any{"filePath": "run.sh", "line": float64(1),
			"before": "GetById", "after": "GetDataKeyById"},
	}}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := applyRenamePlan(io.Discard, dir, plan, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Compare against what the file ACTUALLY had, not a literal 0755:
	// Windows has no Unix permission bits, so the create mode is not the
	// mode you read back. "Unchanged" is the real contract and it is the
	// one that is portable.
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("mode changed: %v -> %v", before.Mode().Perm(), after.Mode().Perm())
	}
}
