package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeInvoker returns canned lookup results per symbol name.
func fakeInvoker(resolves map[string]bool) Invoker {
	return func(tool string, args map[string]any) (any, error) {
		switch tool {
		case "prism_index":
			return map[string]any{}, nil
		case "prism_lookup":
			name, _ := args["name"].(string)
			if resolves[name] {
				return map[string]any{"symbol": map[string]any{"qualifiedName": name}, "content": "x"}, nil
			}
			return map[string]any{"symbol": nil, "matched": false, "name": name}, nil
		}
		return map[string]any{}, nil
	}
}

func newObligationSession(resolves map[string]bool) *Session {
	s := &Session{}
	s.invoke = fakeInvoker(resolves)
	return s
}

// The measured failure: plan Status->StatusCode applied, then the inverse
// applied — tree back at Status. The LAST plan's obligation reflects the
// inverse, so the ledger alone cannot catch it; but the FIRST registration
// followed by the tree ending at the old name must be unmet when only the
// task's plan was applied. This test pins the core contract: obligation
// unmet when the rename is not in effect.
func TestRenameObligation_UnmetWhenTreeKeptOldName(t *testing.T) {
	s := newObligationSession(map[string]bool{"ResponseWriter.Status": true}) // old still resolves, new does not
	s.registerRenameObligation(map[string]any{"query": "ResponseWriter.Status", "newName": "StatusCode"})
	unmet := s.checkObligations()
	if len(unmet) != 1 {
		t.Fatalf("expected 1 unmet obligation, got %v", unmet)
	}
	if !strings.Contains(unmet[0], "StatusCode does not resolve") {
		t.Errorf("unmet detail should name the missing new symbol: %q", unmet[0])
	}
}

func TestRenameObligation_MetWhenRenameInEffect(t *testing.T) {
	s := newObligationSession(map[string]bool{"ResponseWriter.StatusCode": true})
	s.registerRenameObligation(map[string]any{"query": "ResponseWriter.Status", "newName": "StatusCode"})
	if unmet := s.checkObligations(); len(unmet) != 0 {
		t.Fatalf("rename in effect must satisfy the obligation, got %v", unmet)
	}
}

func TestRenameObligation_IncompleteWhenBothResolve(t *testing.T) {
	s := newObligationSession(map[string]bool{
		"ResponseWriter.Status": true, "ResponseWriter.StatusCode": true})
	s.registerRenameObligation(map[string]any{"query": "ResponseWriter.Status", "newName": "StatusCode"})
	unmet := s.checkObligations()
	if len(unmet) != 1 || !strings.Contains(unmet[0], "incomplete") {
		t.Fatalf("both names resolving = incomplete rename, got %v", unmet)
	}
}

func TestRenameObligation_FailsOpenOnEngineError(t *testing.T) {
	s := &Session{}
	s.invoke = func(tool string, args map[string]any) (any, error) {
		if tool == "prism_lookup" {
			return nil, errTest
		}
		return map[string]any{}, nil
	}
	s.registerRenameObligation(map[string]any{"query": "A.b", "newName": "c"})
	if unmet := s.checkObligations(); len(unmet) != 0 {
		t.Fatalf("engine errors must not become fabrication accusations, got %v", unmet)
	}
}

func TestRenameObligation_LastPlanWinsForSamePair(t *testing.T) {
	// Two registrations (plan then inverse) must leave ONE rename obligation
	// reflecting the last applied plan, not two contradictory ones.
	s := newObligationSession(map[string]bool{"ResponseWriter.Status": true})
	s.registerRenameObligation(map[string]any{"query": "ResponseWriter.Status", "newName": "StatusCode"})
	s.registerRenameObligation(map[string]any{"query": "ResponseWriter.StatusCode", "newName": "Status"})
	n := 0
	for _, o := range s.obligations {
		if strings.HasPrefix(o.desc, "rename: ") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one rename obligation after re-registration, got %d", n)
	}
	// The inverse is now in effect (old=StatusCode gone, new=Status resolves):
	// obligations pass — the fingerprint net-zero guard, not the ledger, is
	// what catches the undone task. That split is deliberate.
	if unmet := s.checkObligations(); len(unmet) != 0 {
		t.Fatalf("last-plan obligation should be met in this state, got %v", unmet)
	}
}

var errTest = errFake{}

type errFake struct{}

func (errFake) Error() string { return "engine down" }

// The blind spot that let the measured fabrication through: the .shale
// evidence trail grows every turn, so including it in the fingerprint made
// "tree changed?" always true. Harness artifacts must not count.
func TestTreeFingerprint_IgnoresHarnessArtifacts(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		if out, err := execCommand(root, args...); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "commit", "-q", "--allow-empty", "-m", "base", "--no-verify")
	fp1 := treeFingerprint(root)
	if err := osMkdirAll(root+"/.shale/local", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := osWriteFile(root+"/.shale/local/trail.jsonl", []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fp2 := treeFingerprint(root); fp1 != fp2 {
		t.Fatal("a growing .shale trail must not read as a tree change")
	}
	if err := osWriteFile(root+"/real.go", []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fp3 := treeFingerprint(root); fp3 == fp1 {
		t.Fatal("a real file change must still change the fingerprint")
	}
}

func execCommand(dir string, args ...string) (string, error) {
	c := exec.Command(args[0], args[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	return string(out), err
}

func osMkdirAll(p string, m os.FileMode) error  { return os.MkdirAll(p, m) }
func osWriteFile(p string, b []byte, m os.FileMode) error { return os.WriteFile(p, b, m) }
