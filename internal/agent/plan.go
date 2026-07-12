package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// Plan mode makes the session read-only: the model can explore with every
// read tool (graph ops, read_file, grep, list_files, web_fetch) but every
// mutating path — edit_file, write_file, apply_rename_plan, MCP calls, and
// non-read-only bash — is refused by the HARNESS, not by asking the model
// nicely. The refusal text redirects the model to present a plan instead.

// SetPlan toggles plan (read-only) mode mid-session.
func (s *Session) SetPlan(on bool) { s.plan = on }

// Plan reports whether plan mode is on.
func (s *Session) Plan() bool { return s.plan }

// planNote is folded into each task while plan mode is on.
const planNote = "[PLAN MODE is ON — the session is READ-ONLY. Investigate with the read " +
	"tools (graph ops, read_file, grep, list_files), then reply with a concrete " +
	"step-by-step plan: files to change, edits to make, how to verify. Do NOT " +
	"attempt edits, writes, or state-changing commands — the harness will refuse them.]"

// errPlan is the uniform refusal a mutating tool returns in plan mode.
func errPlan(action string) error {
	return fmt.Errorf("plan mode is ON (read-only): %s refused — present the change as part of your plan instead; the user can run /plan off to apply it", action)
}

// planSafeBash permits only obviously read-only shell commands in plan mode.
// Conservative by design: compound commands, redirection, and pipes are all
// refused — the dedicated read tools cover exploration.
var planSafeFirstWord = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "rg": true, "find": true, "pwd": true, "which": true,
	"file": true, "stat": true, "du": true, "tree": true, "env": true,
	"go": false, // handled below: only `go env`/`go version`
}

var planUnsafeShellRe = regexp.MustCompile(`[|&;><` + "`" + `$]`)

func planSafeBash(command string) bool {
	c := strings.TrimSpace(command)
	if c == "" || planUnsafeShellRe.MatchString(c) {
		return false
	}
	fields := strings.Fields(c)
	switch fields[0] {
	case "git":
		if len(fields) < 2 {
			return false
		}
		switch fields[1] {
		case "status", "log", "diff", "show", "branch", "blame", "ls-files", "remote":
			return true
		}
		return false
	case "go":
		return len(fields) >= 2 && (fields[1] == "env" || fields[1] == "version")
	default:
		return planSafeFirstWord[fields[0]]
	}
}
