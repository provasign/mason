// Package agent is mason's core loop: a multi-turn coding agent whose graph
// operations (prism/grove) and evidence trail (shale) are baked in
// structurally rather than requested by steering files. Graph-op payloads
// render to the user and never enter the model's context; rename edits are
// applied by the harness, not the model; graph-shaped tasks are walled onto
// the graph tools on their first turn.
package agent

import (
	"context"
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
	Root         string
	Out          io.Writer
	MaxTurns     int                      // per Ask; default 30
	Permit       func(action string) bool // gate for bash/edit/write; nil = allow
	ProjectNotes string                   // AGENTS.md/MASON.md content appended to the system prompt
	CtxChars     int                      // history size that triggers auto-compaction (chars); default 400k
	Stream       bool                     // stream assistant text as it arrives (providers that support it)
	Color        bool                     // ANSI styling for the interactive terminal
	Depth        int                      // 0 = top-level; subagents run at 1 and cannot recurse
	NewProvider  func(spec string) (provider.Provider, error) // for subagent model overrides
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
	mutated        bool  // any mutating tool ran during the current Ask
	usageIn        int   // session-total input tokens
	usageOut       int   // session-total output tokens
	st             style // terminal styling
}

func New(p provider.Provider, invoke Invoker, opts Options) *Session {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.MaxTurns == 0 {
		opts.MaxTurns = 30
	}
	if opts.CtxChars == 0 {
		opts.CtxChars = 400_000
	}
	sys := systemPrompt
	if opts.ProjectNotes != "" {
		sys += "\n\nProject instructions (from the repository's AGENTS.md/MASON.md):\n" + opts.ProjectNotes
	}
	return &Session{
		provider: p,
		invoke:   invoke,
		root:     opts.Root,
		out:      opts.Out,
		opts:     opts,
		st:       style{on: opts.Color},
		msgs:     []provider.Msg{{Role: "system", Content: sys}},
	}
}

// chat runs one model turn, streaming text to the terminal when the provider
// supports it. Returns the reply and whether its text already reached the
// screen (so callers don't print it twice).
func (s *Session) chat(ctx context.Context, tools []provider.ToolDef, force bool) (provider.Msg, bool, error) {
	if s.opts.Stream {
		if str, ok := s.provider.(provider.Streamer); ok {
			shown := false
			reply, err := str.ChatStream(ctx, s.msgs, tools, force, func(delta string) {
				if !shown {
					fmt.Fprintln(s.out)
					shown = true
				}
				fmt.Fprint(s.out, delta)
			})
			if shown {
				fmt.Fprintln(s.out)
			}
			return reply, shown, err
		}
	}
	reply, err := s.provider.Chat(ctx, s.msgs, tools, force)
	return reply, false, err
}

// SetProvider switches the model mid-session; the conversation carries over.
func (s *Session) SetProvider(p provider.Provider) { s.provider = p }

// Usage returns session-total input/output tokens across all API calls.
func (s *Session) Usage() (in, out int) { return s.usageIn, s.usageOut }

// History returns the conversation (system prompt included) for persistence.
func (s *Session) History() []provider.Msg { return s.msgs }

// SetHistory restores a persisted conversation. The current system prompt is
// kept (it may have been improved since the session was saved).
func (s *Session) SetHistory(msgs []provider.Msg) {
	restored := []provider.Msg{s.msgs[0]}
	for _, m := range msgs {
		if m.Role != "system" {
			restored = append(restored, m)
		}
	}
	s.msgs = restored
}

// Clear drops the conversation, keeping only the system prompt.
func (s *Session) Clear() { s.msgs = s.msgs[:1] }

// historyChars is the compaction pressure metric (≈ tokens × 4).
func (s *Session) historyChars() int {
	n := 0
	for _, m := range s.msgs {
		n += len(m.Content)
	}
	return n
}

// Compact summarizes everything but the last few messages into one message,
// preserving the system prompt. Returns the chars before/after.
func (s *Session) Compact() (before, after int, err error) {
	before = s.historyChars()
	if len(s.msgs) < 6 {
		return before, before, nil
	}
	keepTail := 3
	head := s.msgs[1 : len(s.msgs)-keepTail]
	var b strings.Builder
	for _, m := range head {
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, truncate(m.Content, 2000))
		for _, c := range m.Calls {
			fmt.Fprintf(&b, "[%s called %s]\n", m.Role, c.Name)
		}
	}
	reply, cerr := s.provider.Chat(context.Background(), []provider.Msg{
		{Role: "system", Content: "You compact coding-session history. Output ONLY a dense summary: the task(s), decisions made, files read/changed, verification results, and open items. No preamble."},
		{Role: "user", Content: b.String()},
	}, nil, false)
	if cerr != nil {
		return before, before, cerr
	}
	if reply.Usage != nil {
		s.usageIn += reply.Usage.In
		s.usageOut += reply.Usage.Out
	}
	// A tool-result tail must not dangle without its assistant call turn:
	// drop leading tool msgs from the kept tail.
	tail := s.msgs[len(s.msgs)-keepTail:]
	for len(tail) > 0 && tail[0].Role == "tool" {
		tail = tail[1:]
	}
	compacted := []provider.Msg{s.msgs[0],
		{Role: "user", Content: "Summary of the conversation so far (older turns compacted):\n" + reply.Content}}
	s.msgs = append(compacted, tail...)
	return before, s.historyChars(), nil
}

func (s *Session) permit(action string) bool {
	if s.opts.Permit == nil {
		return true
	}
	return s.opts.Permit(action)
}

// Ask runs one user task to completion within the ongoing conversation and
// returns the model's final text reply. Cancelling ctx stops cleanly after
// the in-flight step: the conversation stays consistent for the next Ask.
func (s *Session) Ask(ctx context.Context, task string) (string, error) {
	s.msgs = append(s.msgs, provider.Msg{Role: "user", Content: task})
	tr := trail.New(s.root, task)
	defer tr.Done()

	tools := toolDefs()
	if s.invoke == nil {
		tools = codingToolsOnly(tools) // engine unavailable — degrade gracefully
	}
	graphOnly := graphToolsOnly(tools)
	wall := s.invoke != nil && graphIntent.MatchString(task)
	s.mutated = false
	nudged := false
	startFP := treeFingerprint(s.root)

	if s.historyChars() > s.opts.CtxChars {
		if before, after, err := s.Compact(); err == nil && after < before {
			fmt.Fprintf(s.out, "  ⋯ auto-compacted history %d → %d chars\n", before, after)
		}
	}

	for turn := 0; turn < s.opts.MaxTurns; turn++ {
		if ctx.Err() != nil {
			// Interrupted: close the turn coherently so the session can go on.
			s.msgs = append(s.msgs, provider.Msg{Role: "user",
				Content: "[the user interrupted this task]"})
			return "", fmt.Errorf("interrupted")
		}
		// Invocation wall: a graph-shaped task's FIRST turn sees only the
		// graph tools and must call one. After that the full set opens up.
		var reply provider.Msg
		var shown bool
		var err error
		if wall && turn == 0 {
			reply, shown, err = s.chat(ctx, graphOnly, true)
		} else {
			reply, shown, err = s.chat(ctx, tools, false)
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", s.provider.Name(), err)
		}
		if reply.Usage != nil {
			s.usageIn += reply.Usage.In
			s.usageOut += reply.Usage.Out
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
				fmt.Fprintf(s.out, "\n%s\n", s.st.yellow("⚠ mason: this task asked for a change but no file was modified"))
			}
			s.msgs = append(s.msgs, reply)
			if !shown {
				fmt.Fprintf(s.out, "\n%s\n", text)
			}
			// The task mutated the tree: delta-index so the NEXT question is
			// answered from the current graph, not a stale one.
			if s.invoke != nil && s.treeChanged(startFP) {
				_, _ = s.invoke("prism_index", map[string]any{})
			}
			return text, nil
		}
		s.msgs = append(s.msgs, reply)

		for _, call := range reply.Calls {
			result, isErr := s.dispatch(ctx, call, tr)
			content := result
			if isErr != nil {
				content = "error: " + isErr.Error()
			}
			s.msgs = append(s.msgs, provider.Msg{
				Role: "tool", CallID: call.ID, Name: call.Name, Content: content,
			})
		}
	}
	// Out of turns. The work may well be complete (models overshoot
	// investigating); force a tool-less wrap-up instead of reading turn
	// exhaustion as failure — the tree state, not the turn count, is truth.
	s.msgs = append(s.msgs, provider.Msg{Role: "user",
		Content: "Turn limit reached. Stop working NOW. In plain text, summarize what was completed and what (if anything) remains."})
	reply, shown, err := s.chat(ctx, nil, false)
	if err == nil {
		if reply.Usage != nil {
			s.usageIn += reply.Usage.In
			s.usageOut += reply.Usage.Out
		}
		if text := strings.TrimSpace(reply.Content); text != "" {
			s.msgs = append(s.msgs, provider.Msg{Role: "assistant", Content: text})
			if !shown {
				fmt.Fprintf(s.out, "\n%s\n", text)
			}
			fmt.Fprintf(s.out, "%s\n", s.st.yellow("⚠ turn limit reached — verify the summary against the working tree"))
			return text, nil
		}
	}
	return "", fmt.Errorf("gave up after %d turns", s.opts.MaxTurns)
}

// dispatch routes one tool call. Graph ops go through payload isolation;
// coding tools return content to the model.
func (s *Session) dispatch(ctx context.Context, call provider.ToolCall, tr *trail.Trail) (string, error) {
	if _, ok := graphOps[call.Name]; ok {
		fmt.Fprintf(s.out, "  %s\n", s.st.cyan("◆ "+call.Name+" "+compactArgs(call.Args)))
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
		applied, skipped, ambLeft, err := applyRenamePlan(s.out, s.root, s.lastRenamePlan, includeAmbiguous)
		if err != nil {
			return "", err
		}
		s.mutated = true
		tr.Note("applied rename plan (ambiguous=%v)", includeAmbiguous)
		// The model must know the FULL apply state — especially the ambiguous
		// remainder. Without this it hand-fixes the leftover callers one by
		// one (measured: a 30B burned its whole turn budget doing exactly
		// that) instead of making the one call that heals them.
		res := fmt.Sprintf("applied %d edit(s), %d skipped", applied, skipped)
		if ambLeft > 0 {
			res += fmt.Sprintf(". %d AMBIGUOUS caller edits were NOT applied — if the build fails on the renamed symbol, call apply_rename_plan again with includeAmbiguous=true (one call fixes all of them; already-applied lines are skipped automatically). Do NOT edit rename sites by hand", ambLeft)
		}
		return res + ". Now verify with the project's build or tests.", nil
	}

	if call.Name == "subagent" {
		return s.runSubagent(ctx, call, tr)
	}

	if call.Name != "bash" && call.Name != "edit_file" && call.Name != "write_file" {
		fmt.Fprintf(s.out, "  %s\n", s.st.dim("· "+call.Name+" "+compactArgs(call.Args)))
	}
	return s.runCodingTool(ctx, call)
}

// runSubagent delegates a subtask to a fresh Session with an EMPTY context:
// its reads and tool traffic never enter the parent's context — only its
// final summary returns. Depth-limited to one level.
func (s *Session) runSubagent(ctx context.Context, call provider.ToolCall, tr *trail.Trail) (string, error) {
	if s.opts.Depth > 0 {
		return "", fmt.Errorf("subagents cannot spawn subagents — do this work yourself")
	}
	task, _ := call.Args["task"].(string)
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("subagent needs a task")
	}
	p := s.provider
	if spec, _ := call.Args["model"].(string); spec != "" && s.opts.NewProvider != nil {
		np, err := s.opts.NewProvider(spec)
		if err != nil {
			return "", fmt.Errorf("subagent model %q: %w", spec, err)
		}
		p = np
	}
	fmt.Fprintf(s.out, "  %s\n", s.st.cyan("⑂ subagent: "+truncate(task, 100)))
	sub := New(p, s.invoke, Options{
		Root: s.root,
		Out:  &prefixWriter{w: s.out, prefix: "  │ "},
		// Half the parent budget: a runaway subagent must not eat the session.
		MaxTurns: s.opts.MaxTurns / 2,
		Permit:   s.opts.Permit,
		CtxChars: s.opts.CtxChars,
		Color:    s.opts.Color,
		Depth:    s.opts.Depth + 1,
	})
	reply, err := sub.Ask(ctx, task)
	in, out := sub.Usage()
	s.usageIn += in
	s.usageOut += out
	if err != nil {
		return "", fmt.Errorf("subagent: %w", err)
	}
	tr.Note("subagent done: %s", truncate(task, 120))
	return truncate(reply, maxToolOutput), nil
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

// codingToolsOnly strips the graph ops (and read_file's engine dependency is
// handled in the tool itself) when the engine could not be opened.
func codingToolsOnly(all []provider.ToolDef) []provider.ToolDef {
	var out []provider.ToolDef
	for _, t := range all {
		if _, ok := graphOps[t.Name]; ok || t.Name == "apply_rename_plan" {
			continue
		}
		out = append(out, t)
	}
	return out
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
