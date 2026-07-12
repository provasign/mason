# Mason roadmap — competing on proof, not promises

**Positioning:** the only coding CLI where correct tool use is a structural
property of the harness, not a request to the model. Measured result: a $0
local 30B matches Claude Code + Sonnet 3/3 on oracle-verified tasks, and is
faster on graph-shaped work. Save tokens (27–70% ledgered), save time (one
engine call replaces agentic search), increase correctness (closed sets,
compile oracles, quality gates).

## Unique today (keep sharpening)
- Type-resolved graph ops, forced via the invocation wall; relay by construction
- Graph-verified quality gate: untested/dead new code flagged at file:line, fix forced
- Honesty / grounding / refusal guards — the tree, not the model, is truth
- Secret redaction (default-on, zero-leak verified) before model and screen
- Token ledger with SHA-pointer reads; per-session savings shown
- One-keypress local model onboarding, RAM-filtered catalog
- Oracle-verified A/B vs Claude Code (publishable evidence)

## P0 — table stakes to compete (✅ all shipped, v0.14–v0.15)
1. ✅ MCP client (consume GitHub/DB/Slack MCP servers)
2. ✅ Git checkpointing + /undo per task
3. ✅ OpenAI-compatible base-URL provider (LM Studio, llama.cpp, vLLM, OpenRouter)
4. ✅ Sign in with ChatGPT (Codex OAuth — permitted; Anthropic OAuth is banned, keys only)
5. ✅ Permission policies: per-tool/per-path allowlists in .mason/config

## P1 — competitive weight
6. ✅ Plan / read-only mode (v0.16: --plan / /plan, harness-enforced)
7. ✅ `mason init` — generate MASON.md from the graph (v0.16)
8. ✅ Session picker (v0.16: --resume / /sessions / /resume N, named + auto-titled)
9. ✅ Web fetch tool (v0.16: gated, redacted, private-address guarded)
10. ✅ Non-interactive JSON output for CI/SDK (v0.17: --json — one object on stdout)
11. ✅ Cost budgets (v0.17: --max-cost — hard stop, paid-for answers still delivered)
12. LSP diagnostics feed alongside the graph
13. Image input for vision-capable models

## P2 — polish and scale
Custom slash commands + hooks · parallel sessions · themes · first-class
Windows QA · multi-repo workspaces

## Moat-deepening (engine-backed — where we pull away)
- **/review**: engine-verified branch review — change_impact on every changed
  symbol, coverage gaps, dead code, contract breakage. Deterministic PR
  review no prompt-only tool can match.
- **model: auto** — tier routing from our measurements: graph tasks → 14B,
  edits → 30B, hard reasoning → API tier.
- Graph-aware compaction (deterministic context instead of model-written summaries)
- code_context tool (prism_query compressed delivery as direct model food)
- Grove: free-function untested_surface (completes the gate for Python/Go)
- Shale-backed PR descriptions with evidence-verified claims

## Evidence assets (marketing = measurements)
- A/B: mason+30B 3/3 = Claude Code+Sonnet 3/3, $0 vs $0.70, faster on rename
- Tier-invariance study: G* recall 0.997 at every tier incl. free local
- Mode B: recall converts exactly to compile success (0 false claims in 212 runs)
