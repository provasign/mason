// Package agent is mason's core loop: a multi-turn coding agent whose graph
// operations (prism/grove) and evidence trail (shale) are baked in
// structurally rather than requested by steering files. Graph-op payloads
// render to the user and never enter the model's context; rename edits are
// applied by the harness, not the model; graph-shaped tasks are walled onto
// the graph tools on their first turn.
package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/provasign/mason/internal/provider"
	"github.com/provasign/mason/internal/trail"
)

const systemPrompt = `You are mason, a coding agent working in a repository, backed by a
deterministic code graph.

Tool discipline (this is load-bearing — the graph is measured to beat text
search on exactly these shapes):
- "Every site that must change", "all callers", a signature change, or a
  deprecation → change_impact. NEVER grep for callers of a known symbol.
- A rename → rename_plan, then apply_rename_plan. NEVER edit rename sites
  by hand; the harness applies the plan.
- "Who fails to implement X" → missing_implementations.
  "What should I test" → untested_surface. Cleanups → dead_code.
- Graph results render to the user directly; you receive only counts and
  flags. NEVER enumerate graph result sites in your own words.
- read_file may return a short cached-pointer line for a file you already
  read — that means your earlier copy is still valid; do not re-request it.

Working style:
- Look before you leap: read the relevant code before editing.
- After code changes, verify with the project's build or tests via bash.
- Files change ONLY through edit_file, write_file, apply_rename_plan, or a
  bash command. Never claim a change you did not make with a tool call.
- When the task is done, reply in plain text (no tool call) with a short
  summary of what changed and how it was verified.`

// graphIntent detects task shapes that are measured graph-wins; for these
// the first model turn is walled onto the graph tools (invocation wall) so
// routing cannot wander into text search.
var graphIntent = regexp.MustCompile(`(?i)\b(rename|callers?\b|call sites?|change[- ]impact|signature|deprecat|every (site|place|caller|usage)|all (sites|places|callers|usages)|who (implements|breaks)|missing implementation|untested|dead code|unused (code|symbols|functions))`)

// mutationIntent detects tasks that ask for a change to the tree. If such a
// task ends with NO mutating tool having run, the final summary is a
// fabrication — the honesty guard rejects it once and demands the tool call.
var mutationIntent = regexp.MustCompile(`(?i)\b(add|create|fix|change|implement|write|rename|remove|delete|update|refactor|apply|insert|modify)\b`)

// Options configures a Session.
type Options struct {
	Root        string
	Out         io.Writer
	MaxTurns    int          // per Ask; default 30
	Permit      func(action string) bool // gate for bash/edit/write; nil = allow
}

// Session is one conversation against one repository.
type Session struct {
	provider provider.Provider
	invoke   Invoker
	root     string
	out      io.Writer
	opts     Options
	msgs     []provider.Msg

	lastRenamePlan map[string]any
	mutated        bool // any mutating tool ran during the current Ask
}

func New(p provider.Provider, invoke Invoker, opts Options) *Session {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.MaxTurns == 0 {
		opts.MaxTurns = 30
	}
	return &Session{
		provider: p,
		invoke:   invoke,
		root:     opts.Root,
		out:      opts.Out,
		opts:     opts,
		msgs:     []provider.Msg{{Role: "system", Content: systemPrompt}},
	}
}

func (s *Session) permit(action string) bool {
	if s.opts.Permit == nil {
		return true
	}
	return s.opts.Permit(action)
}

// Ask runs one user task to completion within the ongoing conversation and
// returns the model's final text reply.
func (s *Session) Ask(task string) (string, error) {
	s.msgs = append(s.msgs, provider.Msg{Role: "user", Content: task})
	tr := trail.New(s.root, task)
	defer tr.Done()

	tools := toolDefs()
	graphOnly := graphToolsOnly(tools)
	wall := graphIntent.MatchString(task)
	s.mutated = false
	nudged := false
	startFP := treeFingerprint(s.root)

	for turn := 0; turn < s.opts.MaxTurns; turn++ {
		// Invocation wall: a graph-shaped task's FIRST turn sees only the
		// graph tools and must call one. After that the full set opens up.
		var reply provider.Msg
		var err error
		if wall && turn == 0 {
			reply, err = s.provider.Chat(s.msgs, graphOnly, true)
		} else {
			reply, err = s.provider.Chat(s.msgs, tools, false)
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", s.provider.Name(), err)
		}

		if len(reply.Calls) == 0 {
			text := strings.TrimSpace(reply.Content)
			if text == "" {
				s.msgs = append(s.msgs, reply, provider.Msg{Role: "user",
					Content: "Continue: call a tool, or reply with your final summary."})
				continue
			}
			// Honesty guard: a change was requested but the tree is untouched —
			// the summary would be a fabrication. Reject once, demand the tool.
			if mutationIntent.MatchString(task) && !s.treeChanged(startFP) {
				if !nudged {
					nudged = true
					s.msgs = append(s.msgs, reply, provider.Msg{Role: "user",
						Content: "The working tree is unchanged — you have not made the change. " +
							"Make it now with edit_file/write_file (or apply_rename_plan), verify, then summarize. " +
							"If no change is actually needed, say so explicitly instead of claiming one."})
					continue
				}
				fmt.Fprintf(s.out, "\n⚠ mason: this task asked for a change but no file was modified\n")
			}
			s.msgs = append(s.msgs, reply)
			// The task mutated the tree: delta-index so the NEXT question is
			// answered from the current graph, not a stale one.
			if s.invoke != nil && s.treeChanged(startFP) {
				_, _ = s.invoke("prism_index", map[string]any{})
			}
			return text, nil
		}
		s.msgs = append(s.msgs, reply)

		for _, call := range reply.Calls {
			result, isErr := s.dispatch(call, tr)
			content := result
			if isErr != nil {
				content = "error: " + isErr.Error()
			}
			s.msgs = append(s.msgs, provider.Msg{
				Role: "tool", CallID: call.ID, Name: call.Name, Content: content,
			})
		}
	}
	return "", fmt.Errorf("gave up after %d turns", s.opts.MaxTurns)
}

// dispatch routes one tool call. Graph ops go through payload isolation;
// coding tools return content to the model.
func (s *Session) dispatch(call provider.ToolCall, tr *trail.Trail) (string, error) {
	if _, ok := graphOps[call.Name]; ok {
		fmt.Fprintf(s.out, "  ◆ %s %v\n", call.Name, compactArgs(call.Args))
		meta, full, err := runGraphOp(call, s.invoke)
		if err != nil {
			return "", err
		}
		if call.Name == "rename_plan" {
			s.lastRenamePlan = full
		}
		render(s.out, call, full)
		tr.Note("prism %s: %s", call.Name, meta)
		return meta, nil
	}

	if call.Name == "apply_rename_plan" {
		if s.lastRenamePlan == nil {
			return "", fmt.Errorf("no rename_plan has been produced yet")
		}
		includeAmbiguous, _ := call.Args["includeAmbiguous"].(bool)
		if !s.permit("apply rename plan to working tree") {
			return "", fmt.Errorf("user denied apply")
		}
		if err := applyRenamePlan(s.out, s.root, s.lastRenamePlan, includeAmbiguous); err != nil {
			return "", err
		}
		s.mutated = true
		tr.Note("applied rename plan (ambiguous=%v)", includeAmbiguous)
		return "rename plan applied; now verify with the project's build or tests", nil
	}

	if call.Name != "bash" && call.Name != "edit_file" && call.Name != "write_file" {
		fmt.Fprintf(s.out, "  · %s %v\n", call.Name, compactArgs(call.Args))
	}
	return s.runCodingTool(call)
}

// compactArgs renders tool args for the status line without dumping content.
func compactArgs(args map[string]any) string {
	parts := make([]string, 0, len(args))
	for k, v := range args {
		sv := fmt.Sprint(v)
		if len(sv) > 60 {
			sv = sv[:60] + "…"
		}
		parts = append(parts, k+"="+sv)
	}
	return strings.Join(parts, " ")
}

// treeFingerprint captures the working tree's change state (tracked mods +
// untracked paths + diff content). Empty when git is unavailable — the
// guard then falls back to the mutating-tool flag.
func treeFingerprint(root string) string {
	status := exec.Command("git", "-C", root, "status", "--porcelain")
	st, err1 := status.Output()
	diff := exec.Command("git", "-C", root, "diff")
	df, err2 := diff.Output()
	if err1 != nil || err2 != nil {
		return ""
	}
	sum := sha1.Sum(append(st, df...))
	return hex.EncodeToString(sum[:])
}

// treeChanged reports whether the tree moved since startFP (precise, via
// git) or, without git, whether any mutating tool ran (coarse fallback).
func (s *Session) treeChanged(startFP string) bool {
	if startFP == "" {
		return s.mutated
	}
	return treeFingerprint(s.root) != startFP
}

func graphToolsOnly(all []provider.ToolDef) []provider.ToolDef {
	var out []provider.ToolDef
	for _, t := range all {
		if _, ok := graphOps[t.Name]; ok || t.Name == "apply_rename_plan" {
			out = append(out, t)
		}
	}
	return out
}
