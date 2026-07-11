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
mason --continue "follow-up…"  # resume this repo's previous conversation
mason --model ollama:qwen3-coder:30b "task…"
mason --model claude:claude-haiku-4-5-20251001 "task…"
mason --dir ~/src/project --yes "task…"   # --yes skips bash/edit prompts
```

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
`/compact` summarizes old history (also automatic as context fills),
`/clear`, `/help`, `/exit`. Sessions persist per repo; `--continue` resumes.
`AGENTS.md` / `MASON.md` at the root are loaded as project instructions.

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
