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
mason --model ollama:qwen3-coder:30b "task…"
mason --model claude:claude-haiku-4-5-20251001 "task…"
mason --dir ~/src/project --yes "task…"   # --yes skips bash/edit prompts
```

Model auto-detection prefers an installed blessed Ollama model
(qwen3-coder:30b, qwen2.5-coder:14b), then Anthropic, then OpenAI.

### Which model tier do I need?

| Task shape | Tier that suffices |
|---|---|
| Renames, signature changes, callers, missing impls, test gaps, dead code | **local 14B** — measured at the engine ceiling |
| General editing/bugfixing | local 30B or a small API tier (haiku) |

## Credentials

Resolved in order: environment variable (`ANTHROPIC_API_KEY` /
`OPENAI_API_KEY`) → **OS keychain** → interactive prompt (echo off, offer to
store). The keychain (macOS Keychain, Windows Credential Manager, Linux
Secret Service) is the only place mason ever persists a key. Keys are never
written to config files, sessions, logs, or the shale trail, and provider
error paths scrub the key value.

```
mason login anthropic    # store a key in the OS keychain
mason logout anthropic   # remove it
```

## License

Apache-2.0
