# Mason Threat Model

## Assets and boundaries

Mason can read and modify a repository, execute shell commands, call local or
remote models, consume MCP servers, fetch web pages, store sessions and keys,
and create Git checkpoint objects. Assets include source, credentials, working
tree integrity, command authority, model prompts, sessions, and release trust.

Repository instructions, model output, web content, MCP results, provider
responses, and tool output are untrusted. The harness permission layer, path
confinement, keychain, provider boundary, Git worktree, optional Shale process,
and release pipeline are separate trust boundaries.

## Controls

- Mutating tools and fetches are policy-gated; denied paths override `--yes`.
- Tool output is secret-redacted before model delivery.
- Filesystem tools confine paths to the project root.
- Checkpoints use a temporary index and never move HEAD or the user's index.
- Checkpointing fails closed when `.env`, key files, or cloud credential paths
  would enter unreachable Git objects.
- Loopback/private web targets are denied unless explicitly allowed.
- ChatGPT OAuth remains experimental and disabled without an explicit endpoint.

## Retention and residual risk

Sessions persist per repository and may contain source excerpts and model
responses. Unreferenced checkpoint commits persist until Git object pruning.
Shale trails are optional and store compact task/tool metadata. Delete these
artifacts before transferring a repository and never attach them to public
issues without review.

Models can still propose harmful commands, redaction cannot recognize every
secret format, remote providers receive approved context, MCP servers may be
malicious, and undo cannot reverse external side effects. Review permission
previews, use least privilege, and run Mason in an isolated worktree for
untrusted repositories.
