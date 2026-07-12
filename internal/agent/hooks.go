package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Hooks are deterministic shell commands the HARNESS runs around tool
// calls — formatters, guards, notifications — configured in
// .mason/config.json. Like every mason guarantee they live outside the
// model: a pre_bash guard cannot be talked out of blocking.
//
//	{
//	  "hooks": {
//	    "pre_bash":   [{"match": "git push*", "run": "./ci/guard.sh", "block_on_fail": true}],
//	    "post_edit":  [{"match": "*.go", "run": "gofmt -w \"$MASON_FILE\""}],
//	    "post_write": [{"match": "*.go", "run": "gofmt -w \"$MASON_FILE\""}],
//	    "post_task":  [{"run": "afplay /System/Library/Sounds/Glass.aiff"}]
//	  }
//	}
//
// Events: pre_bash (match = the command; a block_on_fail hook that exits
// nonzero refuses the command and feeds its output to the model),
// post_edit / post_write (match = the repo-relative file path; nonzero
// exit becomes a warning in the tool result), post_task (after each Ask).
// Environment: MASON_ROOT, MASON_FILE (post_edit/post_write),
// MASON_COMMAND (pre_bash).
type Hook struct {
	Match       string `json:"match"` // wildcard; empty matches everything
	Run         string `json:"run"`
	BlockOnFail bool   `json:"block_on_fail"`
}

// HookSet maps event name to its hooks.
type HookSet map[string][]Hook

const hookTimeout = 60 * time.Second

// LoadHooks reads the "hooks" section of .mason/config.json (empty set on
// any problem — hooks are opt-in and must never brick a session).
func LoadHooks(root string) HookSet {
	b, err := os.ReadFile(filepath.Join(root, ".mason", "config.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		Hooks HookSet `json:"hooks"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	return cfg.Hooks
}

// runHooks executes every matching hook for an event. subject is what the
// match pattern applies to (the bash command, or the file path). Returns
// the first blocking failure (nil otherwise); non-blocking failures come
// back as warning strings.
func (s *Session) runHooks(ctx context.Context, event, subject string, env map[string]string) (warnings []string, blocked error) {
	hooks := s.opts.Hooks[event]
	for _, h := range hooks {
		if h.Run == "" {
			continue
		}
		if h.Match != "" && !wildMatch(h.Match, subject) {
			continue
		}
		s.setStatus("hook: %s", truncate(h.Run, 50))
		hctx, cancel := context.WithTimeout(ctx, hookTimeout)
		cmd := exec.CommandContext(hctx, "sh", "-c", h.Run)
		cmd.Dir = s.root
		cmd.Env = append(os.Environ(), "MASON_ROOT="+s.root)
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			continue
		}
		head := truncate(strings.TrimSpace(string(out)), 500)
		if h.BlockOnFail {
			fmt.Fprintf(s.out, "  %s\n", s.st.yellow("⛔ hook blocked ("+event+"): "+h.Run))
			return warnings, fmt.Errorf("blocked by %s hook %q: %s", event, h.Run, head)
		}
		fmt.Fprintf(s.out, "  %s\n", s.st.yellow("⚠ hook failed ("+event+"): "+h.Run))
		warnings = append(warnings, fmt.Sprintf("%s hook %q failed: %s", event, h.Run, head))
	}
	return warnings, nil
}

// hookResultSuffix folds non-blocking hook warnings into a tool result.
func hookResultSuffix(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	return "\n\n[hook warnings]\n- " + strings.Join(warnings, "\n- ")
}
