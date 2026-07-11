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
	"path/filepath"
	"regexp"
	"sort"
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
- NEVER ask the user for information you can obtain yourself: the repository
  is in front of you — list_files, read_file, and grep answer questions
  about the project. Read first, then answer.
- When the task is done, reply in plain text (no tool call) with a short
  summary of what changed and how it was verified.`

// graphIntent detects task shapes that are measured graph-wins; for these
// the first model turn is walled onto the graph tools (invocation wall) so
// routing cannot wander into text search.
var graphIntent = regexp.MustCompile(`(?i)\b(rename|callers?\b|call sites?|change[- ]impact|signature|deprecat|every (site|place|caller|usage)|all (sites|places|callers|usages)|who (implements|breaks)|missing implementation|untested|dead code|unused (code|symbols|functions))`)

// refusal detects the lazy-model failure: answering "I don't have enough
// information / could you provide the code" when the repository — and the
// tools to read it — are right there. Fired only when ZERO tools ran during
// the task, so honest "I looked and can't tell" answers pass through.
var refusal = regexp.MustCompile(`(?i)(don'?t|do not|doesn'?t|cannot|can'?t) have (enough|sufficient|access)|without (being provided|seeing|access)|would need (to see|access|more|the)|(if you (can|could) |please |could you )(share|provide|give)|i (cannot|can'?t) (determine|describe|tell|see|access)|need more (information|context|details)`)

// repoIntent detects questions ABOUT the repository — those answers must be
// grounded in tool output, not the model's imagination (measured: a 3B
// invented a whole multi-language data platform for a 3-file Python demo).
var repoIntent = regexp.MustCompile(`(?i)\b(th(is|e)|current|our) +(project|repo(sitory)?|codebase|code base|app(lication)?|package|module)\b|\bin (this|the) (code|repo|project)\b`)

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
	Render       func(string) string      // optional final-text formatter (e.g. markdown → ANSI)
	PermitDetail func(action, detail string) bool // permission gate WITH a preview (diffs); falls back to Permit
	Depth        int                      // 0 = top-level; subagents run at 1 and cannot recurse
	NewProvider  func(spec string) (provider.Provider, error) // for subagent model overrides
	FileSymbols  func(path string) []SymbolInfo // engine hook: indexed symbols of a repo-relative file
	Status       func(activity string)          // live activity narration for the UI status bar
	NoRedact     bool                           // disable secret redaction (default ON)
}

// SetRedact toggles secret redaction mid-session (the /secrets command).
func (s *Session) SetRedact(on bool) { s.opts.NoRedact = !on }

// SymbolInfo is one indexed symbol, provider-neutral (mirrors kit.FileSymbol).
type SymbolInfo struct {
	Name          string
	QualifiedName string
	Kind          string
	Line          int
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
// preserving the system prompt. Returns the chars before/after. Honors ctx —
// compaction is a model call and can take minutes on a loaded machine, and
// an uncancellable compaction makes Ctrl+C appear completely dead.
func (s *Session) Compact(ctx context.Context) (before, after int, err error) {
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
	reply, cerr := s.provider.Chat(ctx, []provider.Msg{
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

// interrupted closes the current turn coherently after a cancellation: the
// marker is ASSISTANT-role so the next Ask's user message alternates —
// consecutive user turns are rejected by the Anthropic API (local models
// tolerate them, which is exactly how this class of bug hides).
func (s *Session) interrupted() error {
	if n := len(s.msgs); n > 0 && s.msgs[n-1].Role == "assistant" {
		return fmt.Errorf("interrupted")
	}
	s.msgs = append(s.msgs, provider.Msg{Role: "assistant",
		Content: "[task interrupted by the user before completion]"})
	return fmt.Errorf("interrupted")
}

// setStatus narrates the agent's current activity to the UI (nil-safe).
func (s *Session) setStatus(format string, args ...any) {
	if s.opts.Status != nil {
		s.opts.Status(fmt.Sprintf(format, args...))
	}
}

func (s *Session) permit(action string) bool {
	if s.opts.Permit == nil {
		return true
	}
	return s.opts.Permit(action)
}

// permitDetail gates an action while SHOWING the user what it will do —
// the diff for an edit, the content head for a write, the counts for a
// rename apply. Falls back to the plain gate when no detail handler is set.
func (s *Session) permitDetail(action, detail string) bool {
	if !s.opts.NoRedact {
		detail, _ = redactSecrets(detail)
	}
	if s.opts.PermitDetail != nil {
		return s.opts.PermitDetail(action, detail)
	}
	return s.permit(action)
}

// renderFinal applies the optional formatter (markdown → ANSI in the TUI)
// to a final assistant reply.
func (s *Session) renderFinal(text string) string {
	if s.opts.Render != nil {
		return s.opts.Render(text)
	}
	return text
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
	refusalNudged := false
	groundingNudged := false
	qualityNudged := false
	ranTests := false
	toolsRan := 0
	startFP := treeFingerprint(s.root)
	startStatus := porcelainStatus(s.root)

	// A task that names existing files gets their content ATTACHED by the
	// harness before the first model turn: weak models refuse to call
	// read_file ("could you share the code?") and Ollama's forced tool
	// choice is best-effort — deterministic pre-seeding removes the model
	// from that loop entirely.
	wallAny := false
	if mentioned := s.filesMentioned(task); len(mentioned) > 0 {
		wallAny = true
		for _, rel := range mentioned {
			if content := s.readForContext(rel); content != "" {
				fmt.Fprintf(s.out, "  %s\n", s.st.dim("· attached "+rel))
				s.msgs = append(s.msgs, provider.Msg{Role: "user",
					Content: "[mason attached " + rel + ", which the task mentions]\n" + s.redact(content)})
			}
		}
	}

	if s.historyChars() > s.opts.CtxChars {
		s.setStatus("compacting history…")
		fmt.Fprintf(s.out, "  ⋯ compacting history…\n")
		if before, after, err := s.Compact(ctx); err == nil && after < before {
			fmt.Fprintf(s.out, "  ⋯ auto-compacted history %d → %d chars\n", before, after)
		} else if ctx.Err() != nil {
			return "", s.interrupted()
		}
	}

	for turn := 0; turn < s.opts.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return "", s.interrupted()
		}
		// Invocation wall: a graph-shaped task's FIRST turn sees only the
		// graph tools and must call one. After that the full set opens up.
		if turn == 0 {
			s.setStatus("waiting for %s…", s.provider.Name())
		} else {
			s.setStatus("waiting for %s… (turn %d)", s.provider.Name(), turn+1)
		}
		var reply provider.Msg
		var shown bool
		var err error
		if wall && turn == 0 {
			reply, shown, err = s.chat(ctx, graphOnly, true)
		} else if wallAny && turn == 0 {
			reply, shown, err = s.chat(ctx, tools, true)
		} else {
			reply, shown, err = s.chat(ctx, tools, false)
		}
		if err != nil {
			if ctx.Err() != nil {
				// Ctrl+C aborted the in-flight call — not a provider fault.
				return "", s.interrupted()
			}
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
			// Refusal guard: the model asked the USER for information while
			// having used none of its tools — bounce it back once.
			if toolsRan == 0 && refusal.MatchString(text) && !refusalNudged {
				refusalNudged = true
				s.msgs = append(s.msgs, reply, provider.Msg{Role: "user",
					Content: "Do not ask me — you have tools. Use list_files, read_file, and " +
						"grep to answer from the repository itself, then give the answer. " +
						"Only if the repository truly cannot answer it, say exactly what you checked."})
				continue
			}
			// Grounding guard: a question about the repository answered with
			// ZERO tool calls is imagination, not inspection. Bounce once;
			// flag visibly if the model insists.
			if toolsRan == 0 && !wallAny && repoIntent.MatchString(task) {
				if !groundingNudged {
					groundingNudged = true
					s.msgs = append(s.msgs, reply, provider.Msg{Role: "user",
						Content: "You used no tools — that answer cannot be grounded in this " +
							"repository. Use list_files and read_file to inspect it, then " +
							"answer from what you actually find. Do not invent details."})
					continue
				}
				fmt.Fprintf(s.out, "\n%s\n", s.st.yellow("⚠ mason: this answer used no tools — it may not reflect the actual repository"))
			}
			// Quality gate: the tree changed — scan what changed for
			// placeholder tests and unimplemented stubs, check that written
			// tests were actually RUN, and ask the ENGINE about the new
			// symbols (graph-verified coverage beats textual patterns).
			if s.treeChanged(startFP) {
				// Reindex first so the graph reflects what the task wrote.
				s.setStatus("quality gate: checking what changed…")
				if s.invoke != nil {
					_, _ = s.invoke("prism_index", map[string]any{})
				}
				changed := changedFilesSince(s.root, startStatus)
				findings := scanQuality(s.root, changed)
				untested, deadNew := s.engineChecks(changed)
				testsInScope := touchedTests(changed) || strings.Contains(strings.ToLower(task), "test")
				if testsInScope {
					for _, u := range untested {
						findings = append(findings, u)
					}
				}
				testsNotRun := touchedTests(changed) && !ranTests
				if (len(findings) > 0 || testsNotRun) && !qualityNudged {
					qualityNudged = true
					var b strings.Builder
					b.WriteString("Quality gate — this work is not done:\n")
					for _, f := range findings {
						b.WriteString("  - " + f.String() + "\n")
					}
					if testsNotRun {
						b.WriteString("  - tests were written or changed but NEVER RUN\n")
					}
					b.WriteString("Fix each item now: replace placeholder tests with real assertions " +
						"about actual behavior, implement or remove stubs, then RUN the test " +
						"suite with bash and report the real results.")
					s.msgs = append(s.msgs, reply, provider.Msg{Role: "user", Content: b.String()})
					continue
				}
				if len(findings) > 0 || testsNotRun {
					fmt.Fprintf(s.out, "\n%s\n", s.st.yellow("⚠ mason quality gate — remaining issues:"))
					for _, f := range findings {
						fmt.Fprintf(s.out, "%s\n", s.st.yellow("  · "+f.String()))
					}
					if testsNotRun {
						fmt.Fprintf(s.out, "%s\n", s.st.yellow("  · tests were written but never run"))
					}
				}
				if !testsInScope && len(untested) > 0 {
					fmt.Fprintf(s.out, "%s\n", s.st.yellow("◆ graph: new code without test coverage (engine-verified):"))
					for _, u := range untested {
						fmt.Fprintf(s.out, "%s\n", s.st.yellow("  · "+u.String()))
					}
				}
				for _, d := range deadNew {
					fmt.Fprintf(s.out, "%s\n", s.st.yellow("◆ graph: "+d.String()))
				}
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
				fmt.Fprintf(s.out, "\n%s\n", s.renderFinal(text))
			}
			return text, nil
		}
		s.msgs = append(s.msgs, reply)

		for _, call := range reply.Calls {
			toolsRan++
			if call.Name == "bash" {
				if cmdStr, _ := call.Args["command"].(string); testRunRe.MatchString(cmdStr) {
					ranTests = true
				}
			}
			result, isErr := s.dispatch(ctx, call, tr)
			content := s.redact(result)
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
				fmt.Fprintf(s.out, "\n%s\n", s.renderFinal(text))
			}
			fmt.Fprintf(s.out, "%s\n", s.st.yellow(fmt.Sprintf(
				"⚠ hit the %d-turn budget — the work may be complete, but this summary is unverified; check the tree (raise with --max-turns)", s.opts.MaxTurns)))
			return text, nil
		}
	}
	return "", fmt.Errorf("gave up after %d turns", s.opts.MaxTurns)
}

// dispatch routes one tool call. Graph ops go through payload isolation;
// coding tools return content to the model.
func (s *Session) dispatch(ctx context.Context, call provider.ToolCall, tr *trail.Trail) (string, error) {
	if _, ok := graphOps[call.Name]; ok {
		s.setStatus("graph: %s %s", call.Name, compactArgs(call.Args))
		fmt.Fprintf(s.out, "  %s\n", s.st.cyan("◆ "+call.Name+" "+compactArgs(call.Args)))
		meta, full, err := runGraphOp(call, s.invoke)
		if err != nil {
			return "", err
		}
		if call.Name == "rename_plan" {
			s.lastRenamePlan = full
		}
		render(s.out, call, full, s.st)
		tr.Note("prism %s: %s", call.Name, meta)
		return meta, nil
	}

	if call.Name == "apply_rename_plan" {
		s.setStatus("applying rename plan…")
		if s.lastRenamePlan == nil {
			return "", fmt.Errorf("no rename_plan has been produced yet")
		}
		includeAmbiguous, _ := call.Args["includeAmbiguous"].(bool)
		nEdits := len(asSlice(s.lastRenamePlan["edits"]))
		nAmb := len(asSlice(s.lastRenamePlan["ambiguous"]))
		detail := fmt.Sprintf("%d confirmed edit(s)", nEdits)
		if includeAmbiguous {
			detail += fmt.Sprintf(" + %d ambiguous", nAmb)
		} else if nAmb > 0 {
			detail += fmt.Sprintf(" (%d ambiguous NOT included)", nAmb)
		}
		detail += " — full plan listed above"
		if !s.permitDetail("apply rename plan to working tree", detail) {
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
	s.setStatus("subagent: %s", truncate(task, 60))
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

// engineChecks interrogates the code graph about a task's changed files:
// which new functions have no test coverage (untested_surface, closed-set)
// and which are unreachable from any entry point (dead_code). Bounded and
// best-effort — engine hiccups must never block a task.
func (s *Session) engineChecks(changed []string) (untested, deadNew []qualityFinding) {
	if s.invoke == nil || s.opts.FileSymbols == nil || len(changed) == 0 || len(changed) > 10 {
		return nil, nil
	}
	changedSet := map[string]bool{}
	var prodFiles []string
	for _, f := range changed {
		changedSet[f] = true
		if !testFileRe.MatchString(f) && isSourceFile(f) {
			prodFiles = append(prodFiles, f)
		}
	}
	if len(prodFiles) > 5 {
		prodFiles = prodFiles[:5]
	}
	checked := 0
	for _, f := range prodFiles {
		for _, sym := range s.opts.FileSymbols(f) {
			if checked >= 8 {
				break
			}
			k := strings.ToLower(sym.Kind)
			if k != "function" && k != "method" {
				continue
			}
			q := sym.QualifiedName
			if q == "" {
				q = sym.Name
			}
			checked++
			res, err := s.invoke("prism_untested_surface", map[string]any{"query": q})
			if err == nil {
				full, _ := res.(map[string]any)
				if full != nil && len(asSlice(full["untested"])) > 0 && len(asSlice(full["covered"])) == 0 {
					untested = append(untested, qualityFinding{file: f, line: sym.Line,
						what: q + " has NO test coverage (engine-verified, closed set)"})
				}
				continue
			}
			// untested_surface is type/method-scoped and rejects FREE
			// functions (most of a fresh Python/Go project; engine follow-up
			// filed) — fall back to a deterministic scan: does ANY test file
			// in the repo reference this name?
			if s.noTestReferences(sym.Name) {
				untested = append(untested, qualityFinding{file: f, line: sym.Line,
					what: q + " is referenced by no test file"})
			}
		}
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
				deadNew = append(deadNew, qualityFinding{file: fp,
					what: name + " was written but is unreachable from any entry point — wire it in or remove it"})
				if len(deadNew) >= 6 {
					break
				}
			}
		}
	}
	return untested, deadNew
}

// noTestReferences scans the repository's test files for a word-boundary
// mention of name. Bounded; errs on the side of silence.
func (s *Session) noTestReferences(name string) bool {
	if len(name) < 3 { // too generic to judge
		return false
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(name) + `\b`)
	if err != nil {
		return false
	}
	referenced := false
	seen := 0
	_ = filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || referenced {
			return filepath.SkipAll
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".grove", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		seen++
		if seen > 20000 {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(s.root, p)
		if !testFileRe.MatchString(rel) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil && re.Match(b) {
			referenced = true
			return filepath.SkipAll
		}
		return nil
	})
	return !referenced
}

// isSourceFile filters the engine checks to code the graph indexes.
func isSourceFile(f string) bool {
	switch strings.ToLower(filepath.Ext(f)) {
	case ".go", ".py", ".java", ".ts", ".tsx", ".js", ".jsx", ".rb", ".cs", ".cpp", ".c", ".rs", ".php":
		return true
	}
	return false
}

// filesMentioned resolves file names the task refers to — exact paths
// first, then basename search anywhere in the repo (bounded) so "what does
// main.py do" finds src/main.py. Returns repo-relative paths, max 3.
func (s *Session) filesMentioned(task string) []string {
	var out []string
	var names []string
	for _, w := range strings.Fields(task) {
		w = strings.Trim(w, `"'.,;:()?!`+"`")
		if !strings.Contains(w, ".") || strings.Contains(w, "..") {
			continue
		}
		if abs, err := s.inRoot(w); err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				if rel, err := filepath.Rel(s.root, abs); err == nil {
					out = append(out, rel)
					continue
				}
			}
		}
		names = append(names, filepath.Base(w))
	}
	if len(names) > 0 {
		want := map[string]bool{}
		for _, n := range names {
			want[n] = true
		}
		seen := 0
		_ = filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
			if err != nil || len(want) == 0 {
				return filepath.SkipAll
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", ".grove", "node_modules", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			seen++
			if seen > 20000 {
				return filepath.SkipAll
			}
			if want[d.Name()] {
				delete(want, d.Name())
				if rel, err := filepath.Rel(s.root, p); err == nil {
					out = append(out, rel)
				}
			}
			return nil
		})
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// readForContext fetches a file for pre-seeding: through prism_read when
// the engine is up (ledgered, SHA-deduped), plain read otherwise. Capped.
func (s *Session) readForContext(rel string) string {
	if s.invoke != nil {
		if res, err := s.invoke("prism_read", map[string]any{"file": rel}); err == nil {
			if m, ok := res.(map[string]any); ok {
				if c, _ := m["content"].(string); c != "" {
					return truncate(c, maxToolOutput)
				}
			}
		}
	}
	abs, err := s.inRoot(rel)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	return truncate(string(b), maxToolOutput)
}

// treeFingerprint captures the working tree's change state: tracked mods
// via git diff, plus the per-file status map (which carries size+mtime for
// untracked files — edits inside untracked files must move the
// fingerprint). Empty when git is unavailable — the guard then falls back
// to the mutating-tool flag.
func treeFingerprint(root string) string {
	stMap := porcelainStatus(root)
	if len(stMap) == 0 {
		// distinguish "clean repo" from "no git": probe git itself
		if err := exec.Command("git", "-C", root, "rev-parse", "--git-dir").Run(); err != nil {
			return ""
		}
	}
	keys := make([]string, 0, len(stMap))
	for k := range stMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "\x00" + stMap[k] + "\n")
	}
	df, err := exec.Command("git", "-C", root, "diff").Output()
	if err != nil {
		return ""
	}
	sum := sha1.Sum(append([]byte(b.String()), df...))
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
