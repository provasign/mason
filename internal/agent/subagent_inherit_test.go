package agent

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// Every option that CONSTRAINS the parent must reach the subagent. A subagent
// runs the same tools against the same tree, so an option the parent is held
// to and the child is not is a hole in that option — a policy that denies
// `git push` denied nothing if the model could delegate the push, and hooks
// stopped being deterministic the moment work moved one level down.
//
// This reads runSubagent's source because the alternative is asserting on a
// Session the caller never gets a handle to; a rename that drops a field is
// exactly the regression worth catching, and the FieldByName check below
// keeps the list itself honest.
func TestSubagentInheritsConstrainingOptions(t *testing.T) {
	constraining := []string{
		"Policy",      // standing deny/allow rules
		"Hooks",       // deterministic pre/post shell hooks
		"NoRedact",    // secret redaction
		"Permit",      // interactive gate
		"MaxCostUSD",  // spend ceiling
		"CostFn",      // ...and the estimator that enforces it
		"Diagnostics", // LSP feedback at edit time
	}
	optT := reflect.TypeOf(Options{})
	for _, name := range constraining {
		if _, ok := optT.FieldByName(name); !ok {
			t.Fatalf("Options has no field %q — update this test and runSubagent together", name)
		}
	}

	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "func (s *Session) runSubagent(")
	if start < 0 {
		t.Fatal("runSubagent not found in agent.go")
	}
	rest := string(src)[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit runSubagent")
	}
	body := rest[:end]

	for _, name := range constraining {
		if !strings.Contains(body, name+":") {
			t.Errorf("runSubagent does not pass Options.%s to the subagent — "+
				"the child escapes a constraint the parent is held to", name)
		}
	}
	// The child spends from the same wallet: its cache tokens must land in
	// the parent's ledger or a subagent's cost is invisible to --max-cost.
	if !strings.Contains(body, "sub.CacheUsage()") {
		t.Error("runSubagent ignores the subagent's cache-token usage")
	}
}
