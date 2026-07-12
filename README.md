# mason

A coding agent that works with **any model** — local (Ollama), Anthropic, or
OpenAI — with the [prism](https://github.com/provasign/prism)/grove code graph
and the shale evidence trail **baked in**. No steering files: the behaviors
that make agents accurate and cheap are properties of the harness, not
requests to the model.

```
mason "Rename the Status method of the ResponseWriter interface to StatusCode.
       Apply the plan including ambiguous edits, then verify with 'go build ./...'"
```

That task runs end-to-end — complete type-resolved rename plan, 24 edits
applied, build verified — on a **local 14B model at $0**.

## Why a harness instead of steering

Measured across model tiers (see the provasign research), steered agents fail
in two ways that prompts cannot fix: they **relay** engine results lossily
(re-typing a site list drops sites), and they **re-derive** solved traversals
through grep. mason makes both structurally impossible:

- **Relay fidelity by construction** — graph-operation payloads (site lists,
  edit plans) render directly to you; the model receives only counts and
  flags. It cannot drop what it never holds.
- **Invocation wall** — tasks shaped like measured graph-wins (renames,
  signature changes, "all callers", dead code) are walled onto the graph
  tools for their first turn. Routing cannot wander into text search.
- **Harness-applied edits** — rename plans are applied by mason with per-line
  drift checks, never re-typed by the model.
- **Graph-aware reads** — `read_file` is prism's session-compressed read: a
  repeat read of an unchanged file costs a ~10-token pointer instead of the
  body. Savings are ledgered and shown after each session.
- **Honesty guard** — if a task asked for a change and the working tree is
  untouched when the model claims success, the claim is rejected once and
  any remaining fabrication is flagged to you, never silently accepted.
- **Evidence trail** — when [shale](https://github.com/provasign/shale) is on
  PATH, every task logs intent → tool notes → done.

## Install

```
go install github.com/provasign/mason/cmd/mason@latest   # or grab a release binary
```

## Use

```
mason                          # interactive REPL in the current repo
mason "task…"                  # one-shot
mason --continue "follow-up…"  # resume this repo's latest conversation
mason --resume                 # pick any saved conversation (list + prompt)
mason --plan "how would we…"   # plan mode: read-only, mutations refused by the harness
mason init                     # generate MASON.md (project map) from the tree + code graph
mason --model sonnet "task…"   # friendly names: sonnet · haiku · opus · gpt · gpt-mini
mason --model ollama:qwen3-coder:30b "task…"           # full specs still work
mason --model openrouter:qwen/qwen3-coder "task…"      # OpenRouter
mason --model lmstudio:qwen2.5-coder-32b "task…"       # LM Studio local server
mason --model oai:http://localhost:8000#my-model "task…" # any OpenAI-compatible server
mason --dir ~/src/project --yes "task…"   # --yes skips bash/edit prompts
mason --json --yes "task…"     # CI/SDK: one JSON object on stdout (reply, usage, changedFiles)
mason --max-cost 0.50 "task…"  # hard cost budget — the task stops at the estimate
```

`--json` keeps stdout machine-clean (narration goes to stderr) and reports
`ok`, `reply`, `usage` (tokens + estimated cost), `changedFiles` (from git),
and `durationMs` — pipe it straight into `jq` in a pipeline. `--max-cost`
is enforced by the harness between model calls: a finished answer that is
already paid for is still delivered; pending tool work is not started.

Per-task **checkpointing**: every task snapshots the tree first (tracked +
untracked, via unreferenced git objects — your HEAD/index never move);
`/undo` reverts the last task's file changes.

**Standing permissions** in `.mason/config.json` — allow-listed commands run
without prompting, denied paths are hard lines that even `--yes` cannot
cross:

```json
{"permissions": {
  "bash": "ask", "edit": "allow", "write": "ask",
  "bash_allow": ["go test *", "go build *", "git status"],
  "bash_deny":  ["git push *"],
  "paths_deny": [".env*", "secrets/**", "*.pem"],
  "fetch": "ask",
  "fetch_allow": ["https://pkg.go.dev/*"]
}}
```

**Plan mode** (`--plan`, or `/plan` in a session) makes the session
structurally read-only: the agent investigates with every read tool and
answers with a concrete plan, while `edit_file`, `write_file`,
`apply_rename_plan`, MCP calls, and non-read-only bash are refused **by the
harness** — not by asking the model to behave. `/plan off` re-enables edits
to apply the plan.

**Web fetch**: the model can pull an http(s) page as plain text
(`web_fetch`) for docs and changelogs — permission-gated like bash,
secret-redacted like everything else, 30s/2MB bounded, and
private/loopback addresses are refused unless `fetch_allow` explicitly
lists them.

**Image input**: name a screenshot in the task ("why does shot.png look
broken?") and mason attaches it to the message — base64, harness-side, on
all three provider wire formats (Ollama / Anthropic / OpenAI-compatible).
`--image <path>` attaches explicitly (repeatable). Needs a vision-capable
model; png/jpg/gif/webp, 8 MB bound.

**Custom slash commands**: every `.mason/commands/<name>.md` becomes
`/<name>` in the REPL and TUI — the file body is the prompt,
`$ARGUMENTS` is replaced with whatever follows the command. Edits apply
instantly (loaded fresh per use); `/help` lists them.

**Hooks** — deterministic shell commands the *harness* runs around tool
calls, configured under `"hooks"` in `.mason/config.json`:

```json
{"hooks": {
  "pre_bash":   [{"match": "git push*", "run": "./ci/guard.sh", "block_on_fail": true}],
  "post_edit":  [{"match": "*.go", "run": "gofmt -w \"$MASON_FILE\""}],
  "post_task":  [{"run": "afplay /System/Library/Sounds/Glass.aiff"}]
}}
```

A `block_on_fail` pre_bash guard refuses the command and feeds its output
to the model — it cannot be talked out of it. post_edit/post_write run
formatters after each write (failures become warnings in the tool result);
post_task fires when a task ends. Env: `MASON_ROOT`, `MASON_FILE`,
`MASON_COMMAND`.

**LSP diagnostics at edit time**: mason auto-detects the project's language
server (gopls, typescript-language-server, pyright/pylsp, rust-analyzer —
whichever is installed), starts it lazily on the first edit, and pipes its
errors/warnings for each written file **into the same tool result as the
edit**. The model sees `undefined: frobnicate` the moment it writes the
call — not three turns later through a failing build. Configure or disable
under `"lsp"` in `.mason/config.json`
(`{"lsp": {"command": "gopls"}}` / `{"lsp": {"disabled": true}}`).

## Free local models — zero setup knowledge required

```
mason models
```

shows what is already installed, what your machine can run (filtered by
memory), and downloads a pick on a single keypress — including installing
the Ollama runtime itself if it's missing. If you start mason with no model
anywhere, it offers this setup instead of an error. `/models` does the same
inside the REPL. The catalog is curated to coding models with tool-calling
support (the measured floor for driving mason); ★ entries are measured in
the provasign study.

Model auto-detection: best installed local model first (catalog order),
then any installed local model, then Anthropic, then OpenAI.

**Nobody types model IDs.** `/models` shows ONE numbered list — installed
local models, downloadable local models, a curated API shortlist (Claude
Fable/Sonnet/Haiku/Opus, GPT-4.1/-mini), and — for any vendor you've
already keyed — **every current model straight from that vendor's own
live model-list API**, so the picker can never go stale the way a
hand-maintained table would. Friendly names work everywhere a spec does:
`--model sonnet`, `/model haiku`, `fable`, `opus`, `gpt`, `gpt-mini`.
Picking an API model without a stored key starts a guided setup right
there: the vendor's key page opens in your browser, you paste the key
(input hidden — in the TUI it's collected masked in the input box), and
it lands only in the OS keychain (macOS Keychain / Windows Credential
Manager / Linux Secret Service). Next time it just switches.

**Slash-command autocomplete.** Type `/` in the TUI and a popup lists
every matching command — built-in and project-defined (`.mason/commands/`)
— with a one-line description, narrowing as you type. ↑/↓ moves the
highlight, Tab/Enter fills it in (a second Enter runs it — the filled
command still needs its arguments), Esc dismisses. The line-mode REPL
gets the same command set via Tab-completion.

**Native text selection.** The TUI captures the mouse for wheel-scrolling,
which normally means you need Shift+drag to select text. `/mouse off`
releases mouse capture — drag-select works exactly like a normal terminal
(PgUp/PgDn still scroll); `/mouse on` (or `/mouse`) re-enables the wheel.

Works on an existing repo or an **empty directory** — "start a brand new Go
project with a module, a package, and tests" scaffolds, builds, and tests a
real project from nothing (verified E2E with a local model at $0).

Interactive sessions open a **full-screen TUI**: scrolling transcript,
input box, spinner, and a live status bar (state · tokens · cost · model).
Assistant replies render as **markdown**. Permission prompts render inline
(y/n) **with a preview of the exact change** — the −/+ diff for an edit,
new-file/overwrite disclosure for a write, edit counts for a rename apply.
`/models` numbers downloadable models too: picking one hands the screen to
ollama's progress bars, then switches to the model automatically. Ctrl+C
cancels the running task without killing the session, ↑/↓/PgUp/PgDn
scroll history. `--no-tui`
keeps the plain line-based REPL (also used automatically when stdout is
not a terminal). Assistant text **streams** as it is generated (Ollama,
Anthropic, OpenAI). In either mode:
`/model <spec>` switches models mid-conversation, `/cost` shows session
tokens and an estimated $ figure, `/savings` the graph-read ledger,
`/compact` condenses old history (also automatic as context fills) —
**deterministically**: the harness builds a factual ledger of what happened
(tasks, tool calls, result heads, replies), no summarizing model call, no
cost, nothing misremembered. `/plan` toggles read-only mode, `/clear`,
`/help`, `/exit`.

**`code_context`** gives the model one-call task context from the graph:
the symbols matching its terms plus their callers, callees, and tests,
compressed to budget — replacing a chain of read_file/grep round-trips
(measured 52% token savings vs raw reads on its first live use).

**Sessions persist per repo** — every conversation is kept. `--continue`
resumes the latest; `mason --resume` (or `/sessions` + `/resume N` inside a
session) lists them with auto-titles and lets you pick; `/resume name <x>`
names the current one. `mason sessions` prints the list headlessly.
`AGENTS.md` / `MASON.md` at the root are loaded as project instructions —
`mason init` generates a MASON.md project map (layout, build/test commands,
entry points, graph stats) deterministically from the tree and code graph.

**Subagents**: the model can delegate a self-contained subtask ("survey how
X is structured", an isolated analysis) to a fresh agent with its own empty
context and the same tools. Only the subagent's final summary returns —
its intermediate reads never consume the parent's context. One level deep,
half the parent's turn budget, optional cheaper model
(`model: ollama:qwen2.5-coder:14b`) for exploration.

### Which model tier do I need?

| Task shape | Tier that suffices |
|---|---|
| Renames, signature changes, callers, missing impls, test gaps, dead code | **local 14B** — measured at the engine ceiling |
| General editing/bugfixing | local 30B or a small API tier (haiku) |

## Production behavior

- **Retries**: transient provider failures (429/5xx/network) retry with
  backoff; auth and bad-request errors fail fast. Streaming retries only
  until the first byte has been shown.
- **Ctrl+C** cancels the current task, not the session; the conversation
  stays consistent and is saved.
- **bash timeout**: one hung command cannot hang mason (default 5m,
  `MASON_BASH_TIMEOUT` seconds to change).
- **Path confinement**: model-supplied paths for read/edit/write/grep/list
  cannot escape the project root.
- **Engine-optional**: if the code graph cannot open, mason warns and runs
  with the coding tools — it never bricks.
- **Turn limit** ends in a forced wrap-up summary flagged for verification,
  not a hard failure; crashes save the session before exiting.

## Credentials

Local models need no credentials. For paid APIs, keys resolve in order:
environment variable (`ANTHROPIC_API_KEY` / `OPENAI_API_KEY`) → **OS
keychain** → interactive prompt (echo off, offer to store). The keychain (macOS Keychain, Windows Credential Manager, Linux
Secret Service) is the only place mason ever persists a key. Keys are never
written to config files, sessions, logs, or the shale trail, and provider
error paths scrub the key value.

```
mason login anthropic    # store a key in the OS keychain
mason logout anthropic   # remove it
```

## License

Apache-2.0
